// Package main is the composition root for plex-language-sync.
//
// Responsibilities of this file:
//   - main() entry point + health subcommand dispatch.
//   - run(): construct the admin Plex client, cache, user manager,
//     syncer, and scheduler, wire them together, start the WebSocket
//     listener, and orchestrate a bounded shutdown join.
//   - notifyAdapter: the thin glue between internal/notify's WebSocket
//     listener and internal/sync. It gates on cfg.triggerOnPlay /
//     cfg.triggerOnScan and forwards relevant events to the syncer.
//
// Env-var contract, HH:MM parsing, and _FILE-suffix secret handling
// live in config.go. Business logic lives under internal/{streams,
// plex, cache, notify, users, sync, scheduler}.
package main

import (
	"context"
	"errors"
	"log/slog"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/health"
	"github.com/cplieger/plex-language-sync/internal/api"
	"github.com/cplieger/plex-language-sync/internal/cache"
	"github.com/cplieger/plex-language-sync/internal/ignore"
	"github.com/cplieger/plex-language-sync/internal/notify"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/scheduler"
	"github.com/cplieger/plex-language-sync/internal/streams"
	syncpkg "github.com/cplieger/plex-language-sync/internal/sync"
	"github.com/cplieger/plex-language-sync/internal/users"
)

// Compile-time interface satisfaction assertions for concrete types
// whose defining packages cannot import api (import cycle).
var _ api.PlexReadWriter = (*plex.Client)(nil)

// cachePath is the on-disk location for the persisted cache. Frozen by
// inviolate contract item 7 (file paths).
const cachePath = "/config/cache.json"

// shutdownWaitBudget bounds how long run() waits for background loops
// (user-token refresh + scheduler) to join before persisting the cache
// on shutdown. If the budget is exceeded the cache is saved anyway — a
// stale-by-10s cache beats a clean-but-unsaved one.
const shutdownWaitBudget = 10 * time.Second

// ---------------------------------------------------------------------------
// Entry point
// ---------------------------------------------------------------------------

func main() {
	if len(os.Args) > 1 && os.Args[1] == "health" {
		health.RunProbe(health.DefaultPath)
	}
	os.Exit(run())
}

func run() int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg := loadConfig()
	logConfig(&cfg)

	marker := health.NewMarker(health.DefaultPath)
	marker.Set(false)

	client, err := plex.NewClient(cfg.plexURL, cfg.plexToken, cfg.caCertPath)
	if err != nil {
		slog.Error("invalid PLEX_URL", "error", err)
		return 1
	}
	plex.WarnIfPlaintextURL(client.BaseURL())

	// Verify connectivity.
	identity, err := client.ServerIdentity(ctx)
	if err != nil {
		slog.Error("cannot connect to plex server", "error", err)
		return 1
	}
	slog.Info("connected to plex server",
		"name", identity.FriendlyName,
		"id", identity.MachineIdentifier,
		"version", identity.Version)

	// Resolve the admin user.
	admin, err := client.LoggedUser(ctx)
	if err != nil {
		slog.Error("cannot resolve admin user", "error", err)
		return 1
	}
	slog.Info("authenticated as admin user", "name", admin.Name, "id", admin.ID)

	// Cache — load persistent state, or start fresh on error.
	c := cache.New()
	if err := c.LoadFrom(cachePath); err != nil {
		slog.Warn("cache load failed, starting fresh", "error", err)
	}
	// Reap any temp orphaned by an interrupted SaveTo so they don't accumulate
	// on the persistent /config volume. Best-effort: a cleanup failure is
	// non-fatal at startup, so log at Debug and continue.
	if _, err := atomicfile.CleanupStaleTemps(filepath.Dir(cachePath), time.Hour); err != nil {
		slog.Debug("stale temp cleanup failed", "error", err)
	}

	// User manager — admin identity + cached shared-user tokens.
	um := users.NewManager(c)
	um.Init(admin, client.BaseURL(), cfg.caCertPath)
	um.LoadFromCache()

	// Synchronous initial refresh with bounded exponential backoff. See
	// internal/users/refresh.go for the retry semantics and rationale.
	um.InitialRefreshWithRetry(ctx, client, identity.MachineIdentifier, users.DefaultRefreshConfig())

	marker.Set(true)
	// Shutdown sequence: flag unhealthy first so Docker stops routing health
	// probes as passing while the (slow) cache save runs, then persist the
	// cache. Set(false) removes the marker, so no separate Cleanup is needed.
	// A failed save here loses the latest learned language profiles and user
	// tokens, so it is logged at Error (operator-actionable), not the Warn used
	// for transient mid-run save failures.
	defer func() {
		marker.Set(false)
		if err := c.SaveTo(cachePath); err != nil {
			slog.Error("cache save on shutdown failed, profiles may be lost",
				"path", cachePath, "error", err)
		}
	}()

	// Compose the sync and scheduler subsystems from the concrete
	// internal/* packages, passing api.* interfaces so the subsystems
	// stay testable.
	userClientFn := func(userID string) api.PlexReadWriter {
		return um.ClientForUser(userID, client)
	}
	ignorePolicy := ignore.NewPolicy(cfg.ignoreLibraries, cfg.ignoreLabels)
	syncer := syncpkg.NewSyncer(
		syncpkg.Config{
			UpdateLevel:      cfg.updateLevel,
			UpdateStrategy:   cfg.updateStrategy,
			Ignore:           ignorePolicy,
			LanguageProfiles: cfg.languageProfiles,
		},
		client,
		c,
		um,
		userClientFn,
	)
	sched := scheduler.New(
		scheduler.Config{
			ScheduleTime:     cfg.schedulerTime,
			Enable:           cfg.schedulerEnable,
			Ignore:           ignorePolicy,
			LanguageProfiles: cfg.languageProfiles,
		},
		client,
		c,
		um,
		userClientFn,
		syncer,
		func() error { return c.SaveTo(cachePath) },
	)

	// runtime-concurrency-p2: join on RefreshLoop + scheduler.Run at
	// shutdown so any in-flight work (a tick mid-analysis, a token
	// refresh mid-HTTP) completes before the deferred cache save
	// writes its final snapshot.
	var wg sync.WaitGroup
	wg.Add(2)
	go func() {
		defer wg.Done()
		um.RefreshLoop(ctx, client, identity.MachineIdentifier)
	}()
	go func() {
		defer wg.Done()
		sched.Run(ctx)
	}()
	// Bounded wait: once Listen returns we give the background loops
	// up to shutdownWaitBudget to drain. Past that we save the cache
	// anyway (stale-by-budget beats unsaved).
	defer func() {
		done := make(chan struct{})
		go func() { wg.Wait(); close(done) }()
		select {
		case <-done:
		case <-time.After(shutdownWaitBudget):
			slog.Warn("shutdown wait budget exceeded, saving cache anyway",
				"budget", shutdownWaitBudget)
		}
	}()

	// WebSocket listener (blocks until context cancelled).
	notify.NewListener(client, notify.DefaultConfig()).Listen(ctx, notifyAdapter{
		syncer: syncer,
		cfg:    &cfg,
		users:  um,
		admin:  admin,
		client: client,
		cache:  c,
		ignore: ignorePolicy,
	})

	slog.Info("shutting down", "cause", context.Cause(ctx))
	return 0
}

// ---------------------------------------------------------------------------
// WebSocket listener (adapter)
// ---------------------------------------------------------------------------
//
// notifyAdapter is the composition-root glue between the WebSocket
// listener (internal/notify) and the sync subsystem (internal/sync).
// It gates play events on cfg.triggerOnPlay and timeline events on
// cfg.triggerOnScan, then forwards relevant events to the sync
// subsystem. The per-event handlers live here (not in internal/sync)
// because the event shape is notify-package-typed and the
// ignore/dedup rules are a blend of cache state and config — both of
// which are main-package concerns.

type notifyAdapter struct {
	syncer *syncpkg.Syncer
	cfg    *config
	users  *users.Manager
	admin  *plex.User
	client *plex.Client
	cache  *cache.Cache
	ignore api.IgnoreChecker
}

func (n notifyAdapter) OnPlay(ctx context.Context, ev notify.PlayEvent) {
	if !n.cfg.triggerOnPlay {
		return
	}
	n.handlePlayEvent(ctx, ev)
}

func (n notifyAdapter) OnTimeline(ctx context.Context, entries []notify.TimelineEntry) {
	if !n.cfg.triggerOnScan {
		return
	}
	n.handleTimeline(ctx, entries)
}

// handlePlayEvent processes a single play session state notification.
func (n notifyAdapter) handlePlayEvent(ctx context.Context, ev notify.PlayEvent) {
	if !notify.IsRelevantPlayEvent(ev) {
		return
	}

	userID, username := n.resolvePlayEventUser(ctx, ev)

	sessionCacheKey := cache.KeyPrefixSession + userID + ":" + ev.SessionKey
	if n.cache.WasRecentlyProcessed(sessionCacheKey) {
		return
	}

	userClient := n.users.ClientForUser(userID, n.client)
	episode, err := userClient.Episode(ctx, plex.RatingKey(ev.RatingKey))
	if err != nil {
		if !errors.Is(err, plex.ErrNotFound) {
			slog.Debug("play event: failed to fetch episode",
				"key", ev.RatingKey, "user", username, "error", err)
		}
		return
	}
	if episode.Type != plex.TypeEpisode {
		return
	}

	curAudio, curSub := streams.Selected(episode)
	streamKey := notify.BuildStreamCacheKey(userID, ev.RatingKey, streams.ID(curAudio), streams.ID(curSub))
	if n.cache.WasRecentlyProcessed(streamKey) {
		return
	}
	n.cache.MarkProcessed(streamKey)
	n.cache.MarkProcessed(sessionCacheKey)

	slog.Info("play event detected",
		"episode", episode.ShortName(),
		"user", username,
		"state", ev.State)

	n.syncer.ChangeTracksForEpisode(ctx, userClient, userID, episode, "play")
}

// resolvePlayEventUser resolves the user from a play event's client
// identifier. Falls back to the admin user if the session cannot be
// resolved.
func (n notifyAdapter) resolvePlayEventUser(ctx context.Context, ev notify.PlayEvent) (userID, username string) {
	if ev.ClientIdentifier != "" {
		if uid, uname, err := n.client.UserFromSession(ctx, ev.ClientIdentifier); err == nil {
			return uid, uname
		}
		slog.Debug("could not resolve user from session, using admin",
			"client", ev.ClientIdentifier)
	}
	return n.admin.ID, n.admin.Name
}

func (n notifyAdapter) handleTimeline(ctx context.Context, entries []notify.TimelineEntry) {
	for i := range entries {
		entry := &entries[i]
		if !notify.IsRelevantTimelineEntry(entry) {
			continue
		}

		cacheKey := cache.KeyPrefixTimeline + entry.ItemID
		if n.cache.WasRecentlyProcessed(cacheKey) {
			continue
		}

		episode, err := n.client.Episode(ctx, plex.RatingKey(entry.ItemID))
		if err != nil {
			slog.Debug("timeline: failed to fetch episode",
				"id", entry.ItemID, "error", err)
			continue
		}
		if episode.Type != plex.TypeEpisode {
			continue
		}
		if n.ignore != nil && n.ignore.ShouldSkipEpisode(ctx, n.client, episode) {
			continue
		}

		action := notify.TimelineAction(entry)

		slog.Info("library scan event detected",
			"episode", episode.ShortName(),
			"action", action)

		n.cache.MarkProcessed(cacheKey)

		// For new/updated episodes, process for ALL users.
		n.syncer.ProcessNewOrUpdatedEpisodeAllUsers(ctx, episode, action)
	}
}
