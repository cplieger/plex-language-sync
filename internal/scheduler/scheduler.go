// Package scheduler owns the periodic deep-analysis tick and its
// sub-workers (recent-history replay + recently-added sweep).
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
//     (runtime-algorithms-p1) with a circuit breaker that aborts the
//     pass after a threshold of consecutive per-item failures.
//   - Persist the last-run marker through api.Cache so a cold restart
//     does not double-run the analysis.
//
// Inviolate contracts preserved (see refactor-agent-guide.md):
//   - WARN slog keys ("scheduler: aborting history processing after
//     consecutive failures", "scheduler: failed to fetch history",
//     "scheduler: failed to fetch sections", "scheduler: deep analysis
//     already in progress, skipping") byte-for-byte identical
//     (inviolate item 5).
//   - INFO slog keys ("scheduler enabled", "scheduled deep analysis
//     starting", "running initial deep analysis", "deep analysis
//     completed", "scheduler: processing recently added episode",
//     "scheduler stopped") identical.
//   - /config/cache.json schema unchanged — LastSchedulerRun reads and
//     writes go through api.Cache and are tagged by the concrete
//     internal/cache package (inviolate item 7).
//
// Consumer note: scheduler depends on api.PlexReader, api.Cache,
// api.UserLookup, and a Syncer interface satisfied by *sync.Syncer
// (declared locally to keep this package testable without importing
// internal/sync, which would create a visible dependency direction
// only used in one place). In practice main.go wires the concrete
// *sync.Syncer through.
package scheduler

import (
	"context"
	"errors"
	"log/slog"
	"strconv"
	stdsync "sync"
	"sync/atomic"
	"time"

	"github.com/cplieger/plex-language-sync/internal/api"
	"github.com/cplieger/plex-language-sync/internal/cache"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
	"golang.org/x/sync/singleflight"
)

// deepAnalysisConcurrency is the upper bound on in-flight per-item work
// during a deep-analysis pass (runtime-algorithms-p1). Chosen to keep
// load on the Plex server modest while still shrinking wall-clock time
// for large libraries. A higher value trades responsiveness of the
// Plex server for faster nightly catch-up.
const deepAnalysisConcurrency = 4

// maxConsecutiveErrors is the circuit-breaker threshold shared across
// all workers — once this many per-item failures accumulate without an
// intervening success, the rest of the pass is aborted. Preserves the
// pre-extraction behaviour (5 consecutive errors → abort) but applies
// it across goroutines atomically.
const maxConsecutiveErrors = 5

// Config captures the subset of application configuration the
// Scheduler actually reads. Decoupling from the full main.config keeps
// the package boundary clean and lets tests construct a Scheduler
// without mimicking the app's full env-var surface.
type Config struct {
	Ignore   api.IgnoreChecker // library skip rules; nil means "never skip"
	Interval time.Duration     // deep-analysis cadence; <=0 means disabled
	Enable   bool              // scheduler on/off
}

// CacheSaver is the narrow persistence sink the scheduler needs: a
// single "flush the cache to disk" call invoked at the end of each
// deep-analysis tick. Deliberately separate from api.Cache (which
// deliberately excludes file-system concerns) so the scheduler can
// trigger a disk flush without the api.Cache consumers needing to
// know about the persistence path. *cache.Cache satisfies this via a
// trivial closure in the composition root.
type CacheSaver func() error

// Syncer is the narrow interface the scheduler needs from the sync
// package: a per-user track-apply call plus the multi-user fan-out.
// The ignore-library/ignore-label skip checks live on
// api.IgnoreChecker (injected via Config.Ignore) rather than on the
// Syncer, so overlapping event/scheduler paths share one decision
// point instead of the three duplicated implementations that existed
// pre-extraction.
// *sync.Syncer satisfies this. Declared here (rather than imported)
// to keep scheduler independent of internal/sync for test ergonomics.
type Syncer interface {
	ChangeTracksForEpisode(ctx context.Context, userClient api.PlexReadWriter, userID string, reference *streams.Episode, trigger string)
	ProcessNewOrUpdatedEpisodeAllUsers(ctx context.Context, episode *streams.Episode, trigger string)
}

// Scheduler owns the deep-analysis tick and its workers. Safe for
// concurrent Run calls, but the intended shape is a single Run
// goroutine per process.
//
// runtime-concurrency-p1: concurrent Run invocations collapse their
// overlapping deep-analysis triggers onto a single in-flight run via
// singleflight.Group. Within one Run goroutine the initial catch-up
// and scheduled ticks are already sequential, so the dedup only
// matters when Run is driven from more than one goroutine. The
// runner goroutine that loses the dedup race still logs a WARN with
// the "scheduler: deep analysis already in progress, skipping" key so
// Loki alerts keyed on that string continue to fire (inviolate item 5).
type Scheduler struct {
	plex       api.PlexReader
	cache      api.Cache
	users      api.UserLookup
	sync       Syncer
	dedup      singleflight.Group
	userClient api.UserClientFunc
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

// New constructs a Scheduler with the given collaborators. saveCache
// may be nil in tests that don't exercise the disk-flush path.
func New(cfg Config, reader api.PlexReader, c api.Cache, lookup api.UserLookup, userClient api.UserClientFunc, s Syncer, saveCache CacheSaver) *Scheduler {
	return &Scheduler{
		cfg:        cfg,
		plex:       reader,
		cache:      c,
		users:      lookup,
		userClient: userClient,
		sync:       s,
		saveCache:  saveCache,
	}
}

// Run is the outer scheduler loop: it runs a deep-analysis pass at
// startup when the cache's LastSchedulerRun marker is absent or older
// than one Interval, then runs one every Interval via a time.Ticker.
// Returns when the context is cancelled. A disabled scheduler
// (Enable=false or Interval<=0) returns immediately.
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
	// does not double-run on restart.
	lastRun := s.cache.LastSchedulerRun()
	if lastRun.IsZero() || time.Since(lastRun) > s.cfg.Interval {
		slog.Info("running initial deep analysis")
		s.deepAnalysis(ctx)
	}

	// Fixed-interval scheduling via time.Ticker (the fleet
	// docker-*-scheduler convention). Overlapping ticks collapse via the
	// singleflight in deepAnalysis, so no wall-clock slot-dedup is needed
	// and no local wall-clock time is read.
	ticker := time.NewTicker(s.cfg.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ticker.C:
			slog.Info("scheduled deep analysis starting")
			s.deepAnalysis(ctx)
		case <-ctx.Done():
			return
		}
	}
}

// deepAnalysis runs the 24h replay of history + recently-added sweep,
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
	// progress, skipping") from the pre-singleflight implementation
	// (inviolate item 5).
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
	defer func() {
		s.cache.SetLastSchedulerRun(time.Now())
		if s.saveCache != nil {
			if err := s.saveCache(); err != nil {
				slog.Warn("cache save failed", "error", err)
			}
		}
	}()

	sinceUnix := time.Now().Add(-24 * time.Hour).Unix()
	s.processRecentHistory(ctx, sinceUnix)
	s.processRecentlyAdded(ctx, sinceUnix)

	slog.Info("deep analysis completed",
		"since", time.Unix(sinceUnix, 0).UTC().Format(time.RFC3339))
}

// processRecentHistory replays language settings from the last window
// of play history.
//
// runtime-algorithms-p1: the prior implementation ran a single
// sequential loop with a local counter tracking consecutive failures.
// The new implementation fans out per-item work across a bounded
// worker pool (deepAnalysisConcurrency workers) while preserving the
// same circuit-breaker semantics (maxConsecutiveErrors) using an
// atomic counter shared between workers. Successful items reset the
// counter; once the threshold is reached, every worker returns
// promptly without processing additional items.
func (s *Scheduler) processRecentHistory(ctx context.Context, sinceUnix int64) {
	history, err := s.plex.History(ctx, sinceUnix)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Debug("scheduler: history fetch cancelled during shutdown", "error", err)
			return
		}
		slog.Warn("scheduler: failed to fetch history", "error", err)
		return
	}
	slog.Debug("scheduler: processing recent history",
		"items", len(history))

	work := make(chan plex.HistoryItem)
	var wg stdsync.WaitGroup
	var consecutiveErrors atomic.Int32
	// unknownUsers de-spams the per-item "no per-user client" WARN: each
	// unknown userID is reported once per pass instead of once per history
	// item, so a single unmanaged user with N recent plays no longer emits
	// N identical WARN lines. Created per pass and shared across workers.
	var unknownUsers stdsync.Map

	for range s.workerCount() {
		wg.Go(func() { s.historyWorker(ctx, work, &consecutiveErrors, &unknownUsers) })
	}
	s.feedHistory(ctx, work, history, &consecutiveErrors)
	close(work)
	wg.Wait()
}

// historyWorker drains work until it closes, replaying each history
// item via processHistoryItem. It does no further work once the context
// is cancelled (returning) or the shared circuit breaker has tripped
// (continuing to drain so the feeder can finish and close the channel).
func (s *Scheduler) historyWorker(ctx context.Context, work <-chan plex.HistoryItem, consecutiveErrors *atomic.Int32, unknownUsers *stdsync.Map) {
	for item := range work {
		if ctx.Err() != nil {
			return
		}
		if consecutiveErrors.Load() >= maxConsecutiveErrors {
			// Continue draining the channel so the feeder goroutine can
			// exit; do no further work.
			continue
		}
		s.processHistoryItem(ctx, item, consecutiveErrors, unknownUsers)
	}
}

// feedHistory pushes episode history items into work, applying the same
// pre-filters the sequential implementation used (skip non-episode types
// and ignored libraries so workers are never woken for a no-op) and the
// same circuit breaker: once maxConsecutiveErrors accumulates without an
// intervening success it logs the abort WARN and stops feeding.
func (s *Scheduler) feedHistory(ctx context.Context, work chan<- plex.HistoryItem, history []plex.HistoryItem, consecutiveErrors *atomic.Int32) {
	for _, item := range history {
		if ctx.Err() != nil {
			break
		}
		if n := consecutiveErrors.Load(); n >= maxConsecutiveErrors {
			slog.Warn("scheduler: aborting history processing after consecutive failures",
				"consecutive_errors", n)
			break
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
		}
	}
}

// processHistoryItem runs a single history replay: fetch the per-user
// episode and delegate to the Syncer's ChangeTracksForEpisode. Success
// resets the shared error counter; a fetch failure increments it.
func (s *Scheduler) processHistoryItem(
	ctx context.Context,
	item plex.HistoryItem,
	consecutiveErrors *atomic.Int32,
	unknownUsers *stdsync.Map,
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
		slog.Debug("scheduler: skipping history item, fetch failed",
			"key", item.RatingKey, "user", userID, "error", fetchErr)
		return
	}
	consecutiveErrors.Store(0)
	s.sync.ChangeTracksForEpisode(ctx, userClient, userID, ep, "scheduler")
}

// processRecentlyAdded applies language settings to recently added
// episodes for all users. Fans episodes out across a bounded worker
// pool (see processRecentHistory for the rationale).
func (s *Scheduler) processRecentlyAdded(ctx context.Context, sinceUnix int64) {
	sections, err := s.plex.ShowSections(ctx)
	if err != nil {
		if errors.Is(err, context.Canceled) {
			slog.Debug("scheduler: sections fetch cancelled during shutdown", "error", err)
			return
		}
		slog.Warn("scheduler: failed to fetch sections", "error", err)
		return
	}
	slog.Debug("scheduler: scanning recently added episodes",
		"sections", len(sections))

	// Fan episodes across the worker pool. The feeder iterates sections
	// sequentially (one HTTP call per section to RecentlyAdded) but
	// pushes individual episodes into the work channel so per-episode
	// processing runs concurrently.
	work := make(chan streams.Episode)
	var wg stdsync.WaitGroup

	for range s.workerCount() {
		wg.Go(func() { s.recentlyAddedWorker(ctx, work) })
	}
	s.feedRecentlyAdded(ctx, work, sections, sinceUnix)
	close(work)
	wg.Wait()
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
// work for the pool. Preserves the pre-extraction skip rules (ignored
// libraries, per-section fetch-failure skip) and the section/episode
// ordering.
func (s *Scheduler) feedRecentlyAdded(ctx context.Context, work chan<- streams.Episode, sections []plex.Section, sinceUnix int64) {
	var total, failed int
	for _, section := range sections {
		if ctx.Err() != nil {
			break
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
			break
		}
	}
	if failed > 0 && ctx.Err() == nil {
		slog.Warn("scheduler: recently-added sweep incomplete, some sections failed to fetch",
			"failed_sections", failed, "total_sections", total)
	}
}

// feedEpisodes pushes every episode into work in order, returning false
// if ctx was cancelled before all were sent (signalling the caller to
// stop the sweep). Extracted from feedRecentlyAdded so the section loop
// stays under the cognitive-complexity gate; behaviour (ordering, the
// break-outer-loop-on-cancel via the old `break feed` label) is preserved.
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
// admin reader, then delegates to
// ProcessNewOrUpdatedEpisodeAllUsers, which runs a single reference
// search shared across every user followed by a per-user write path.
// Plex returns identical metadata regardless of token (verified
// 2026-04-26 against live API + Tautulli playback history), so the
// read side is token-independent and writes use the per-user client.
func (s *Scheduler) processRecentlyAddedEpisode(ctx context.Context, ep *streams.Episode) {
	cacheKey := cache.KeyPrefixScheduler + ep.RatingKey
	if !s.cache.CheckAndMark(cacheKey) {
		return
	}

	slog.Info("scheduler: processing recently added episode",
		"episode", ep.ShortName())

	full, fetchErr := s.plex.Episode(ctx, plex.RatingKey(ep.RatingKey))
	if fetchErr != nil {
		if !errors.Is(fetchErr, plex.ErrNotFound) {
			slog.Debug("scheduler: failed to fetch recently added episode",
				"key", ep.RatingKey, "error", fetchErr)
		}
		return
	}
	if s.cfg.Ignore != nil && s.cfg.Ignore.ShouldSkipEpisode(ctx, s.plex, full) {
		return
	}

	s.sync.ProcessNewOrUpdatedEpisodeAllUsers(ctx, full, "scheduler")
}
