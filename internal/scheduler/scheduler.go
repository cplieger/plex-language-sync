// Package scheduler owns the daily deep-analysis tick and its
// sub-workers (recent-history replay + recently-added sweep).
//
// Responsibilities:
//   - Schedule a daily deep-analysis run at an absolute HH:MM boundary
//     (clock-jump-safe: DST, NTP step, container start mid-minute all
//     resolve to the next HH:MM boundary).
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
	"github.com/cplieger/plex-language-sync/internal/timeutil"
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
	Ignore       api.IgnoreChecker // library skip rules; nil means "never skip"
	ScheduleTime string            // "HH:MM"
	Enable       bool              // scheduler on/off
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
// runtime-concurrency-p1: overlapping deep-analysis triggers (e.g. the
// initial catch-up firing close to the first scheduled HH:MM tick)
// collapse onto a single in-flight run via singleflight.Group. The
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

// Run is the outer scheduler loop: it runs the first deep-analysis
// pass if the cache's LastSchedulerRun marker is absent or older than
// 24h, then sleeps until the next HH:MM boundary. Returns when the
// context is cancelled.
func (s *Scheduler) Run(ctx context.Context) {
	defer slog.Info("scheduler stopped")

	if !s.cfg.Enable {
		slog.Info("scheduler disabled")
		return
	}

	slog.Info("scheduler enabled", "time", s.cfg.ScheduleTime)

	// Run immediately if never run before or last run was >24h ago.
	lastRun := s.cache.LastSchedulerRun()
	if lastRun.IsZero() || time.Since(lastRun) > 24*time.Hour {
		slog.Info("running initial deep analysis")
		s.deepAnalysis(ctx)
	}

	// Absolute-target scheduling: one timer per day, aligned to HH:MM.
	// Clock-jump safe (DST, NTP step, container start mid-minute all
	// resolve to the next HH:MM boundary).
	hour, minute, ok := timeutil.ParseHHMM(s.cfg.ScheduleTime)
	if !ok {
		hour, minute = 2, 0
	}
	for {
		next := nextScheduledRun(time.Now(), hour, minute)
		timer := time.NewTimer(time.Until(next))
		select {
		case <-timer.C:
			// Guard against double-runs (e.g. clock rewound via NTP).
			lr := s.cache.LastSchedulerRun()
			if time.Since(lr) > 23*time.Hour {
				slog.Info("scheduled deep analysis starting",
					"scheduled_for", next.Format(time.RFC3339))
				s.deepAnalysis(ctx)
			}
		case <-ctx.Done():
			timer.Stop()
			return
		}
	}
}

// nextScheduledRun returns the next time the scheduler should fire for
// "HH:MM" relative to now. If HH:MM already occurred today, it rolls
// to tomorrow. Uses absolute-target scheduling instead of a minute
// ticker so that DST spring-forward, NTP step corrections, and
// container start mid-minute do not silently skip the run.
func nextScheduledRun(now time.Time, hour, minute int) time.Time {
	target := time.Date(now.Year(), now.Month(), now.Day(),
		hour, minute, 0, 0, now.Location())
	if !target.After(now) {
		target = target.Add(24 * time.Hour)
	}
	return target
}

// deepAnalysis runs the 24h replay of history + recently-added sweep,
// deduplicating concurrent invocations via singleflight so overlapping
// ticks (e.g. initial + scheduled firing close together) collapse to
// a single run. Callers that lose the race receive no error and
// observe a WARN log from this method — they do NOT re-run.
func (s *Scheduler) deepAnalysis(ctx context.Context) {
	// Do returns shared=true when this caller piggybacked on an
	// already-in-flight call. We always return (nil, nil) from the
	// callback so value/err are uninteresting; the only signal we
	// consume is `shared`, which drives the "skipping" log. Preserve
	// the operational-alert key ("scheduler: deep analysis already in
	// progress, skipping") from the pre-singleflight implementation.
	_, err, shared := s.dedup.Do("deep_analysis", func() (any, error) {
		s.deepAnalysisCore(ctx)
		return nil, nil
	})
	_ = err // callback always returns nil
	if shared {
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
		"since", time.Unix(sinceUnix, 0).Format(time.RFC3339))
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
		slog.Warn("scheduler: failed to fetch history", "error", err)
		return
	}
	slog.Debug("scheduler: processing recent history",
		"items", len(history))

	work := make(chan plex.HistoryItem)
	var wg stdsync.WaitGroup
	var consecutiveErrors atomic.Int32

	for range deepAnalysisConcurrency {
		wg.Go(func() {
			for item := range work {
				if ctx.Err() != nil {
					return
				}
				if consecutiveErrors.Load() >= maxConsecutiveErrors {
					// Continue draining the channel so the feeder
					// goroutine below can exit; do no further work.
					continue
				}
				s.processHistoryItem(ctx, item, &consecutiveErrors)
			}
		})
	}

	// Feeder: push items until the channel is full or the breaker
	// trips. Pre-filters items that have no work attached so workers
	// are never woken for a no-op.
	for _, item := range history {
		if ctx.Err() != nil {
			break
		}
		if consecutiveErrors.Load() >= maxConsecutiveErrors {
			slog.Warn("scheduler: aborting history processing after consecutive failures",
				"consecutive_errors", consecutiveErrors.Load())
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
	close(work)
	wg.Wait()
}

// processHistoryItem runs a single history replay: fetch the per-user
// episode and delegate to the Syncer's ChangeTracksForEpisode. Success
// resets the shared error counter; a fetch failure increments it.
func (s *Scheduler) processHistoryItem(
	ctx context.Context,
	item plex.HistoryItem,
	consecutiveErrors *atomic.Int32,
) {
	userID := strconv.Itoa(int(item.AccountID))
	userClient := s.userClient(userID)
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
		slog.Warn("scheduler: failed to fetch sections", "error", err)
		return
	}
	slog.Debug("scheduler: scanning recently added episodes",
		"sections", len(sections))

	// Fan episodes across the worker pool. The feeder below iterates
	// sections sequentially (one HTTP call per section to RecentlyAdded)
	// but pushes individual episodes into the work channel so per-episode
	// processing runs concurrently.
	work := make(chan streams.Episode)
	var wg stdsync.WaitGroup

	for range deepAnalysisConcurrency {
		wg.Go(func() {
			for ep := range work {
				if ctx.Err() != nil {
					return
				}
				epCopy := ep
				s.processRecentlyAddedEpisode(ctx, &epCopy)
			}
		})
	}

	for _, section := range sections {
		if ctx.Err() != nil {
			break
		}
		if s.cfg.Ignore != nil && s.cfg.Ignore.IgnoreLibrary(section.Title) {
			continue
		}
		episodes, err := s.plex.RecentlyAdded(ctx, plex.RatingKey(section.Key), sinceUnix)
		if err != nil {
			slog.Debug("scheduler: failed to fetch recently added",
				"section", section.Title, "error", err)
			continue
		}
		for i := range episodes {
			select {
			case work <- episodes[i]:
			case <-ctx.Done():
			}
		}
	}
	close(work)
	wg.Wait()
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
	if s.cache.WasRecentlyProcessed(cacheKey) {
		return
	}
	s.cache.MarkProcessed(cacheKey)

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
