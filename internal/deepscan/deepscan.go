// Package deepscan owns the periodic deep-analysis tick and its
// sub-workers (recent-history replay + recently-added sweep). Named for what
// it does rather than "scheduler": that name belongs to the first-party
// scheduler library this package consumes, and the collision forced a
// schedlib alias on the library at every use.
//
// Responsibilities:
//   - Schedule a periodic deep-analysis run on a fixed Go-duration
//     interval (default 24h), matching the fleet docker-*-scheduler
//     convention: one pass at startup (when the last run is older than
//     one interval) plus a time.Ticker every interval thereafter. The
//     pass is a safety net over the real-time WebSocket listener, so a
//     drifting wall-clock start hour is immaterial; using an interval
//     rather than an absolute HH:MM boundary means the app reads no
//     local wall-clock time (no TZ / time/tzdata dependency).
//   - Fan out per-item work across a bounded worker pool
//     with a circuit breaker that aborts the
//     pass after a threshold of consecutive per-item failures.
//   - Persist the last-run record in a scheduler.Stamp file on the
//     persistent volume so a cold restart does not double-run the
//     analysis.
//
// Stable contracts preserved (keep these exact: Loki alerts grep the log
// strings):
//   - WARN slog keys ("scheduler: aborting history processing after
//     consecutive failures", "scheduler: failed to fetch history",
//     "scheduler: failed to fetch sections", "scheduler: deep analysis
//     already in progress, skipping") byte-for-byte identical.
//   - INFO slog keys ("scheduler enabled", "scheduled deep analysis
//     starting", "running initial deep analysis", "deep analysis
//     completed", "scheduler: processing recently added episode",
//     "scheduler stopped") identical.
//
// Consumer note: every collaborator is an interface THIS package declares (see
// deps.go) — plexReader, EpisodeReader, runLedger, skipChecker and Syncer, each
// naming only the methods the pass calls. Nothing is imported from a shared
// contract package, and Syncer in particular is declared here rather than
// imported so deepscan needs no dependency on internal/tracksync. In practice
// main.go wires the concrete *tracksync.Syncer through.
package deepscan

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cplieger/plex-language-sync/internal/cache"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/scheduler/v4"
	"golang.org/x/sync/singleflight"
)

// deepAnalysisConcurrency is the upper bound on in-flight per-item work
// during a deep-analysis pass. A higher value trades responsiveness of the
// Plex server for faster nightly catch-up.
const deepAnalysisConcurrency = 4

// maxConsecutiveErrors is the circuit-breaker threshold shared across
// all workers — once this many per-item failures accumulate without an
// intervening success, the rest of the pass is aborted.
const maxConsecutiveErrors = 5

// maxDeepAnalysisLookback caps the dynamic look-back window. The window
// origin is the previous COMPLETED run, so a large DEEP_SCAN_INTERVAL or a
// long outage would otherwise grow it without bound — and History /
// RecentlyAdded are fetched in a single, non-paginated response capped at
// 10 MB, so an unbounded window risks overflowing that cap. The cap bounds
// the window; events older than it are the real-time WebSocket listener's
// responsibility, not this safety net's. 30 days comfortably covers a long
// outage or a multi-day interval while keeping the fetched window bounded.
const maxDeepAnalysisLookback = 30 * 24 * time.Hour

// Config captures the subset of application configuration the
// Scheduler actually reads. Decoupling from the full main.config keeps
// the package boundary clean and lets tests construct a Scheduler
// without mimicking the app's full env-var surface.
type Config struct {
	Ignore   skipChecker   // library skip rules; nil means "never skip"
	Interval time.Duration // deep-analysis cadence; <=0 means disabled
	Enable   bool          // scheduler on/off
}

// Scheduler owns the deep-analysis tick and its workers. Safe for
// concurrent Run calls, but the intended shape is a single Run
// goroutine per process.
//
// Concurrent Run invocations collapse their overlapping deep-analysis
// triggers onto a single in-flight run via singleflight.Group. The
// runner goroutine that loses the dedup race still logs a WARN with
// the "scheduler: deep analysis already in progress, skipping" key so
// Loki alerts keyed on that string continue to fire.
type Scheduler struct {
	plex       plexReader
	cache      runLedger
	sync       Syncer
	stamp      *scheduler.Stamp
	dedup      singleflight.Group
	userClient UserClientFunc
	saveCache  CacheSaver
	cfg        Config
	// workers bounds in-flight per-item work during a deep-analysis
	// pass; zero (the default) means deepAnalysisConcurrency. Exists as
	// a test seam: a serial (workers=1) run lets the breaker's
	// reset-on-success semantics be asserted deterministically, without
	// depending on goroutine scheduling under -race.
	workers int
}

// workerCount is the effective size of the per-item worker pool,
// falling back to deepAnalysisConcurrency when unset (the production
// path). Tests override Scheduler.workers to force a deterministic
// serial run.
func (s *Scheduler) workerCount() int {
	if s.workers < 1 {
		return deepAnalysisConcurrency
	}
	return s.workers
}

// Deps carries New's collaborators. Seven positional parameters, six of them
// interfaces this package declares, is a transposition waiting to happen: the
// compatible method sets among them mean a swapped pair type-checks and the
// pass then reads history through the wrong collaborator. Named fields, wired
// once at the composition root.
type Deps struct {
	// Plex reads history and library metadata for the sweep.
	Plex plexReader
	// Cache holds the dedup keys and the recorded intents the pass re-applies.
	Cache runLedger
	// Stamp is the persisted last-run record: it gates the startup pass and
	// anchors the replay look-back window.
	Stamp *scheduler.Stamp
	// UserClient returns the per-user write client for a username.
	UserClient UserClientFunc
	// Sync applies the recorded intent to an episode.
	Sync Syncer
	// SaveCache flushes the cache to disk. May be nil in tests that do not
	// exercise the disk-flush path.
	SaveCache CacheSaver
}

// New constructs a Scheduler from cfg and deps.
func New(cfg Config, deps Deps) *Scheduler {
	return &Scheduler{
		cfg:        cfg,
		plex:       deps.Plex,
		cache:      deps.Cache,
		stamp:      deps.Stamp,
		userClient: deps.UserClient,
		sync:       deps.Sync,
		saveCache:  deps.SaveCache,
	}
}

// Run is the outer scheduler loop: it runs a deep-analysis pass at
// startup when the last-run stamp is absent or older than one Interval,
// then runs one every Interval via a time.Ticker. Returns when the
// context is cancelled. A disabled scheduler (Enable=false or
// Interval<=0) returns immediately.
func (s *Scheduler) Run(ctx context.Context) {
	defer slog.Info("scheduler stopped")

	if !s.cfg.Enable || s.cfg.Interval <= 0 {
		slog.Info("scheduler disabled")
		return
	}

	slog.Info("scheduler enabled", "interval", s.cfg.Interval.String())

	// Run immediately when never run before or the last run is older than
	// one interval, so a container restarting more often than the interval
	// is never starved of a safety-net pass, while one that ran recently
	// does not double-run on restart. CountFailed because only complete
	// passes are recorded and the ticker owns the retry.
	if s.stamp.Due(s.cfg.Interval, time.Now(), scheduler.CountFailed) {
		slog.Info("running initial deep analysis")
		s.deepAnalysis(ctx)
	}

	// Fixed-interval scheduling via scheduler.RunLoop (the fleet
	// docker-*-scheduler convention). FireOnStart is false: the conditional
	// startup pass above already handled the immediate run (RunLoop's
	// unconditional FireOnStart would ignore the last-run stamp and double-run
	// on a recent restart). Overlapping ticks collapse via the singleflight in
	// deepAnalysis, RunLoop is sequential, so no wall-clock slot-dedup is needed
	// and no local wall-clock time is read.
	scheduler.RunLoop(ctx, func(ctx context.Context) {
		slog.Info("scheduled deep analysis starting")
		s.deepAnalysis(ctx)
	}, scheduler.LoopOptions{Interval: s.cfg.Interval})
}

// deepAnalysis runs the recent-history replay + recently-added sweep,
// deduplicating concurrent invocations via singleflight so overlapping
// ticks (e.g. initial + scheduled firing close together) collapse to
// a single run. Callers that lose the race receive no error and
// observe a WARN log from this method — they do NOT re-run.
func (s *Scheduler) deepAnalysis(ctx context.Context) {
	// singleflight.Do returns shared=true to EVERY caller that received
	// this result once a duplicate joined — including the goroutine
	// that actually executed the callback (Do returns c.dups>0 to the
	// winner, not only to the losers). Guarding the "skipping" alert on
	// `shared` alone therefore makes the winner that DID the work also
	// log the skip under concurrent Run. Track execution with a local
	// flag set inside the callback: only the winner runs the callback,
	// so `executed` stays false on every loser. Preserve the
	// operational-alert key ("scheduler: deep analysis already in
	// progress, skipping") from the earlier implementation.
	executed := false
	_, _, shared := s.dedup.Do("deep_analysis", func() (any, error) {
		executed = true
		s.deepAnalysisCore(ctx)
		return nil, nil
	})
	if shared && !executed {
		slog.Warn("scheduler: deep analysis already in progress, skipping")
	}
}

// deepAnalysisCore is the actual body of a deep-analysis tick. It
// runs exactly once per singleflight key; overlapping callers share
// its completion via dedup.Do.
func (s *Scheduler) deepAnalysisCore(ctx context.Context) {
	complete := false
	defer func() {
		// Record the run ONLY when this pass fully covered its look-back
		// window. The sweeps run newest-first, so any early exit (cancel,
		// breaker abort, fetch/overflow error, per-section failure) leaves
		// the OLDER end unprocessed; recording then would move the next
		// run's look-back origin past those unswept events and drop them
		// permanently. An unrecorded pass re-sweeps the same window, which
		// is idempotent. The outcome bit is informational: the startup gate
		// reads the record under CountFailed, which considers age alone.
		if complete {
			if err := s.stamp.Record(true); err != nil {
				slog.Warn("last-run stamp write failed", "error", err)
			}
		}
		if s.saveCache != nil {
			if err := s.saveCache(); err != nil {
				slog.Warn("cache save failed", "error", err)
			}
		}
	}()

	// Look back to the previous completed run so no window is missed when
	// DEEP_SCAN_INTERVAL exceeds 24h; floor at 24h so a frequent interval
	// (or no record on first boot) still replays a full recent day. The
	// stamp still holds the PREVIOUS run here because this run is only
	// recorded in the deferred Record above, after the body completes.
	lookback := 24 * time.Hour
	rec, _ := s.stamp.Last()
	if last := rec.Time; !last.IsZero() {
		if since := time.Since(last); since > lookback {
			lookback = since
		}
	}
	// Cap the window (see maxDeepAnalysisLookback) so a long outage or a
	// large interval cannot grow the non-paginated 10 MB fetch without bound.
	if lookback > maxDeepAnalysisLookback {
		lookback = maxDeepAnalysisLookback
	}
	sinceUnix := time.Now().Add(-lookback).Unix()

	histComplete := s.processRecentHistory(ctx, sinceUnix)
	addedComplete := s.processRecentlyAdded(ctx, sinceUnix)
	complete = histComplete && addedComplete && ctx.Err() == nil

	slog.Info("deep analysis completed",
		"since", time.Unix(sinceUnix, 0).UTC().Format(time.RFC3339),
		"complete", complete,
		"marker_advanced", complete)
}

// processRecentHistory reconciles the last window of play history:
// for each replayed item the sync layer re-applies the user's RECORDED
// intent (or skips — see ReconcileWithIntent); the replay never derives
// a user's choice from the episode's current selection state.
//
// Fans out per-item work across a bounded worker pool
// (deepAnalysisConcurrency workers), sharing the circuit-breaker
// threshold (maxConsecutiveErrors) via an atomic counter. Successful
// items reset the counter; once the threshold is reached, every worker
// returns promptly without processing additional items.
func (s *Scheduler) processRecentHistory(ctx context.Context, sinceUnix int64) bool {
	history, err := s.plex.History(ctx, sinceUnix)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Debug("scheduler: history fetch cancelled during shutdown", "error", err)
			return false
		}
		slog.Warn("scheduler: failed to fetch history", "error", err)
		return false
	}
	slog.Debug("scheduler: processing recent history",
		"items", len(history))

	work := make(chan plex.HistoryItem)
	var wg sync.WaitGroup
	var consecutiveErrors atomic.Int32
	var totalErrors atomic.Int32
	// unknownUsers de-spams the per-item "no per-user client" WARN: each
	// unknown userID is reported once per pass instead of once per history
	// item, so a single unmanaged user with N recent plays no longer emits
	// N identical WARN lines. Created per pass and shared across workers.
	var unknownUsers sync.Map

	for range s.workerCount() {
		wg.Go(func() { s.historyWorker(ctx, work, &consecutiveErrors, &totalErrors, &unknownUsers) })
	}
	fedAll := s.feedHistory(ctx, work, history, &consecutiveErrors)
	close(work)
	wg.Wait()
	// Mirror feedRecentlyAdded's partial-failure summary so a degraded
	// history replay is visible at WARN. ctx.Err()==nil guard mirrors
	// feedRecentlyAdded so in-flight context.Canceled fetches on shutdown
	// do not raise a false alarm.
	if n := totalErrors.Load(); n > 0 && ctx.Err() == nil {
		slog.Warn("scheduler: recent-history replay incomplete, some items failed to fetch",
			"failed_items", n, "total_items", len(history))
	}
	// Complete iff every item was fed (breaker did not abort, ctx not
	// cancelled). Scattered per-item fetch failures that did not trip the
	// breaker do NOT block completion — they are the design's accepted
	// skip-and-continue loss, already surfaced by the WARN above.
	return fedAll && ctx.Err() == nil
}

// historyWorker drains work until it closes, replaying each history
// item via processHistoryItem. It does no further work once the context
// is cancelled (returning) or the shared circuit breaker has tripped
// (continuing to drain so the feeder can finish and close the channel).
func (s *Scheduler) historyWorker(ctx context.Context, work <-chan plex.HistoryItem, consecutiveErrors, totalErrors *atomic.Int32, unknownUsers *sync.Map) {
	for item := range work {
		if ctx.Err() != nil {
			return
		}
		if consecutiveErrors.Load() >= maxConsecutiveErrors {
			// Continue draining the channel so the feeder goroutine can
			// exit; do no further work.
			continue
		}
		s.processHistoryItem(ctx, item, consecutiveErrors, totalErrors, unknownUsers)
	}
}

// feedHistory pushes episode history items into work, applying the same
// pre-filters the sequential implementation used (skip non-episode types
// and ignored libraries so workers are never woken for a no-op) and the
// same circuit breaker: once maxConsecutiveErrors accumulates without an
// intervening success it logs the abort WARN and stops feeding.
func (s *Scheduler) feedHistory(ctx context.Context, work chan<- plex.HistoryItem, history []plex.HistoryItem, consecutiveErrors *atomic.Int32) bool {
	for _, item := range history {
		if ctx.Err() != nil {
			return false
		}
		if n := consecutiveErrors.Load(); n >= maxConsecutiveErrors {
			slog.Warn("scheduler: aborting history processing after consecutive failures",
				"consecutive_errors", n)
			return false
		}
		if item.Type != plex.TypeEpisode {
			continue
		}
		if s.cfg.Ignore != nil && s.cfg.Ignore.IgnoreLibrary(item.LibraryTitle) {
			continue
		}
		select {
		case work <- item:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// processHistoryItem runs a single history-replay reconciliation: fetch
// the per-user episode (to resolve its show and pass the ignore gate)
// and delegate to the Syncer's ReconcileWithIntent, which re-applies the
// user's recorded intent — or skips when none exists or the play is
// newer than the observation. Success resets the shared error counter;
// a fetch failure increments it.
func (s *Scheduler) processHistoryItem(
	ctx context.Context,
	item plex.HistoryItem,
	consecutiveErrors, totalErrors *atomic.Int32,
	unknownUsers *sync.Map,
) {
	userID := strconv.Itoa(int(item.AccountID))
	userClient := s.userClient(userID)
	if userClient == nil {
		// Log once per unknown user per pass, not once per history item:
		// an unmanaged/removed user's every recent play would otherwise
		// emit a separate WARN and drown genuine signals in Loki.
		if _, seen := unknownUsers.LoadOrStore(userID, struct{}{}); !seen {
			slog.Warn("scheduler: no per-user client, skipping history item",
				"user", userID)
		}
		return
	}
	ep, fetchErr := userClient.Episode(ctx, plex.RatingKey(item.RatingKey))
	if fetchErr != nil {
		consecutiveErrors.Add(1)
		totalErrors.Add(1)
		slog.Debug("scheduler: skipping history item, fetch failed",
			"key", item.RatingKey, "user", userID, "error", fetchErr)
		return
	}
	consecutiveErrors.Store(0)
	s.sync.ReconcileWithIntent(ctx, userID, ep, int64(item.ViewedAt), "scheduler")
}

// processRecentlyAdded applies language settings to recently added
// episodes for all users. Fans episodes out across a bounded worker
// pool (see processRecentHistory for the rationale).
func (s *Scheduler) processRecentlyAdded(ctx context.Context, sinceUnix int64) bool {
	sections, err := s.plex.ShowSections(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Debug("scheduler: sections fetch cancelled during shutdown", "error", err)
			return false
		}
		slog.Warn("scheduler: failed to fetch sections", "error", err)
		return false
	}
	slog.Debug("scheduler: scanning recently added episodes",
		"sections", len(sections))

	// Fan episodes across the worker pool. The feeder iterates sections
	// sequentially (one HTTP call per section to RecentlyAdded) but
	// pushes individual episodes into the work channel so per-episode
	// processing runs concurrently.
	work := make(chan streams.Episode)
	var wg sync.WaitGroup

	for range s.workerCount() {
		wg.Go(func() { s.recentlyAddedWorker(ctx, work) })
	}
	fedAll := s.feedRecentlyAdded(ctx, work, sections, sinceUnix)
	close(work)
	wg.Wait()
	// Complete iff every non-ignored section was fetched and fully fed
	// (no per-section fetch failure, no ctx cancel).
	return fedAll && ctx.Err() == nil
}

// recentlyAddedWorker drains work until it closes, handling each
// recently-added episode for all users. Returns promptly once the
// context is cancelled.
func (s *Scheduler) recentlyAddedWorker(ctx context.Context, work <-chan streams.Episode) {
	for ep := range work {
		if ctx.Err() != nil {
			return
		}
		s.processRecentlyAddedEpisode(ctx, &ep)
	}
}

// feedRecentlyAdded iterates sections sequentially (one RecentlyAdded
// fetch per non-ignored section) and pushes each returned episode into
// work for the pool. Preserves the earlier skip rules (ignored
// libraries, per-section fetch-failure skip) and the section/episode
// ordering.
func (s *Scheduler) feedRecentlyAdded(ctx context.Context, work chan<- streams.Episode, sections []plex.Section, sinceUnix int64) bool {
	var total, failed int
	for _, section := range sections {
		if ctx.Err() != nil {
			return false
		}
		if s.cfg.Ignore != nil && s.cfg.Ignore.IgnoreLibrary(section.Title) {
			continue
		}
		total++
		episodes, err := s.plex.RecentlyAdded(ctx, plex.RatingKey(section.Key), sinceUnix)
		if err != nil {
			failed++
			slog.Debug("scheduler: failed to fetch recently added",
				"section", section.Title, "error", err)
			continue
		}
		if !feedEpisodes(ctx, work, episodes) {
			return false
		}
	}
	if failed > 0 && ctx.Err() == nil {
		slog.Warn("scheduler: recently-added sweep incomplete, some sections failed to fetch",
			"failed_sections", failed, "total_sections", total)
	}
	// A per-section fetch failure leaves that section's window unswept, so
	// the pass is incomplete even though the loop ran to the end.
	return failed == 0 && ctx.Err() == nil
}

// feedEpisodes pushes every episode into work in order, returning false
// if ctx was cancelled before all were sent (signalling the caller to
// stop the sweep).
func feedEpisodes(ctx context.Context, work chan<- streams.Episode, episodes []streams.Episode) bool {
	for i := range episodes {
		select {
		case work <- episodes[i]:
		case <-ctx.Done():
			return false
		}
	}
	return true
}

// processRecentlyAddedEpisode handles a single recently-added episode
// for all users. Fetches the episode's full metadata once via the
// admin reader, then delegates to ProcessNewOrUpdatedEpisodeAllUsers,
// which runs a single reference search shared across every user
// followed by a per-user write path. Plex returns identical metadata
// regardless of token (verified 2026-04-26 against live API + Tautulli
// playback history), so the read side is token-independent and writes
// use the per-user client.
func (s *Scheduler) processRecentlyAddedEpisode(ctx context.Context, ep *streams.Episode) {
	full, fetchErr := s.plex.Episode(ctx, plex.RatingKey(ep.RatingKey))
	if fetchErr != nil {
		if !errors.Is(fetchErr, plex.ErrNotFound) {
			slog.Debug("scheduler: failed to fetch recently added episode",
				"key", ep.RatingKey, "error", fetchErr)
		}
		return
	}
	if s.cfg.Ignore != nil && s.cfg.Ignore.ShouldSkipEpisode(ctx, full) {
		return
	}

	// Mark the dedup key only after a successful fetch + ignore-check.
	// A transient Episode() failure above therefore leaves the key
	// unmarked, so the next scheduler pass retries immediately instead of
	// suppressing the episode for the ~5-minute dedup window. CheckAndMark
	// stays the atomic guard that lets exactly one worker reach
	// ProcessNewOrUpdatedEpisodeAllUsers, so moving it here costs at most a
	// redundant idempotent fetch if the same RatingKey is enqueued twice
	// within the recently-added sweep — never a double write.
	cacheKey := cache.KeyPrefixScheduler + ep.RatingKey
	if !s.cache.CheckAndMark(cacheKey) {
		return
	}

	slog.Info("scheduler: processing recently added episode",
		"episode", ep.ShortName())

	s.sync.ProcessNewOrUpdatedEpisodeAllUsers(ctx, full, "scheduler")
}
