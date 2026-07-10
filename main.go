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
// Env-var contract, SCHEDULER_INTERVAL parsing, and _FILE-suffix secret
// handling live in config.go. Business logic lives under internal/{streams,
// plex, cache, notify, users, sync, scheduler}.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
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
		slog.Error("cannot initialize plex client", "error", err)
		return 1
	}
	plex.WarnIfPlaintextURL(client.BaseURL())

	// Derive the cache encryption key from the admin PLEX_TOKEN up front. It is
	// a pure function of the token (no Plex round-trip) and is deterministic for
	// a given token, so decryption works offline on restart. Computing it here,
	// before the connect loop, means a malformed token is surfaced as a config
	// error BEFORE the loop can mark the container healthy-degraded.
	encKey, err := cache.DeriveKey(cfg.plexToken)
	if err != nil {
		slog.Error("cannot derive encryption key", "error", err)
		return 1
	}

	// Establish the initial Plex connection and resolve the admin user. A
	// transient failure (Plex down or unreachable at boot) marks the container
	// healthy-degraded and keeps retrying rather than crash-looping under the
	// restart policy; a fatal failure (bad token, wrong server, or a TLS/cert
	// misconfiguration) exits non-zero. Blocks until connected, fatal, or a
	// shutdown signal.
	identity, admin, err := connectAndResolveAdmin(ctx, client, marker)
	if err != nil {
		marker.Set(false) // clear any degraded-healthy marker before exiting
		if ctx.Err() != nil {
			slog.Info("shutdown requested during startup", "cause", context.Cause(ctx))
			return 0
		}
		slog.Error("cannot establish initial plex connection", "error", err)
		return 1
	}
	slog.Info("connected to plex server",
		"name", identity.FriendlyName,
		"id", identity.MachineIdentifier,
		"version", identity.Version)
	slog.Info("authenticated as admin user", "name", admin.Name, "id", admin.ID)

	// Cache — load persistent state, or start fresh on error. The encryption
	// key (derived above) makes user-token decryption work offline on restart.
	c := cache.New()
	c.SetEncryptionKey(encKey)
	if err := c.LoadFrom(cachePath); err != nil {
		slog.Warn("cache load failed, starting fresh", "error", err)
	}
	// Reap any temp orphaned by an interrupted SaveTo so they don't accumulate
	// on the persistent /config volume. Best-effort: a cleanup failure is
	// non-fatal at startup, so log at Warn and continue.
	if _, err := atomicfile.CleanupStaleTemps(filepath.Dir(cachePath), time.Hour); err != nil {
		slog.Warn("stale temp cleanup failed", "path", filepath.Dir(cachePath), "error", err)
	}

	// User manager — admin identity + cached shared-user tokens.
	um := users.NewManager(c)
	um.Init(admin, client.BaseURL(), cfg.caCertPath)
	um.LoadFromCache()

	// Connection verified and admin resolved (possibly after a healthy-degraded
	// retry period), cache + user manager initialized: the app can serve.
	// marker.Set(true) is idempotent — the connect loop already set it if the
	// initial connection was degraded; setting it here also covers the
	// connected-on-first-try path. Health = "connected, or transiently retrying
	// the initial connect"; a fatal config/auth error exits before this point.
	// Marked BEFORE the plex.tv shared-user refresh so container liveness is not
	// gated on that secondary dependency: gating on the refresh would delay
	// healthy up to ~75s (DefaultRefreshConfig) on a plex.tv outage and risk a
	// Docker unhealthy/restart that cannot fix plex.tv. The periodic RefreshLoop
	// keeps retrying.
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

	// Synchronous initial refresh with bounded exponential backoff. See
	// internal/users/refresh.go for the retry semantics and rationale. Runs
	// after the health marker is set so a plex.tv outage never gates liveness.
	um.InitialRefreshWithRetry(ctx, client, identity.MachineIdentifier, users.DefaultRefreshConfig())

	// Compose the sync and scheduler subsystems from the concrete
	// internal/* packages, passing api.* interfaces so the subsystems
	// stay testable.
	userClientFn := func(userID string) api.PlexReadWriter {
		// ClientForUser returns a typed nil (*plex.Client)(nil) when no
		// per-user client can be built. Returning that directly would
		// produce a non-nil api.PlexReadWriter interface wrapping a nil
		// pointer (the classic Go nil-interface trap), defeating the
		// consumers' `== nil` checks. Convert to a genuine nil interface
		// so sync/scheduler skip the user instead of dereferencing.
		uc := um.ClientForUser(userID, client)
		if uc == nil {
			return nil
		}
		return uc
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
			Interval: cfg.schedulerInterval,
			Enable:   cfg.schedulerEnabled,
			Ignore:   ignorePolicy,
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
	refreshDone := make(chan struct{})
	schedDone := make(chan struct{})
	wg.Go(func() {
		defer close(refreshDone)
		um.RefreshLoop(ctx, client, identity.MachineIdentifier)
	})
	wg.Go(func() {
		defer close(schedDone)
		sched.Run(ctx)
	})
	// Bounded wait: once Listen returns we give the background loops
	// up to shutdownWaitBudget to drain. Past that we save the cache
	// anyway (stale-by-budget beats unsaved). On timeout we report which
	// loops are still running so a stuck shutdown points at the laggard.
	defer waitForBackgroundLoops(&wg, refreshDone, schedDone)

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

// waitForBackgroundLoops blocks until the user-token-refresh and scheduler
// loops join, or until shutdownWaitBudget elapses, whichever comes first. On
// timeout it logs which loops are still running so a stuck shutdown points at
// the laggard before the deferred cache save runs.
func waitForBackgroundLoops(wg *sync.WaitGroup, refreshDone, schedDone <-chan struct{}) {
	done := make(chan struct{})
	go func() { wg.Wait(); close(done) }()
	select {
	case <-done:
	case <-time.After(shutdownWaitBudget):
		var stuck []string
		select {
		case <-refreshDone:
		default:
			stuck = append(stuck, "user-token-refresh")
		}
		select {
		case <-schedDone:
		default:
			stuck = append(stuck, "scheduler")
		}
		slog.Warn("shutdown wait budget exceeded, saving cache anyway",
			"budget", shutdownWaitBudget, "still_running", stuck)
	}
}

// ---------------------------------------------------------------------------
// Initial connection (degraded start)
// ---------------------------------------------------------------------------
//
// The app cannot do anything without a Plex connection + a resolved admin
// user (the WebSocket listener, scheduler, syncer, and user manager all
// depend on them), so a "degraded start" is: mark healthy, then retry the
// initial connect until it succeeds, rather than serving in a reduced mode.
// This keeps a Plex-down-at-boot from crash-looping the container under the
// restart policy (the old behaviour was os.Exit(1) on the first failure).
// A fatal config/auth error still exits fast so the misconfiguration is loud.

const (
	startupBaseBackoff = 1 * time.Second
	startupMaxBackoff  = 30 * time.Second
)

// connectAndResolveAdmin verifies the Plex connection and resolves the admin
// user, retrying transient failures with capped exponential backoff. On the
// first transient failure it marks the container healthy (a degraded start),
// then keeps retrying until Plex answers. A fatal error (bad token / 4xx, a
// wrong-server 404, or a TLS/cert misconfiguration) returns immediately so the
// caller can exit non-zero. Returns ctx.Err() when shutdown is signalled
// mid-retry.
func connectAndResolveAdmin(ctx context.Context, client *plex.Client, marker *health.Marker) (*plex.ServerIdentity, *plex.User, error) {
	degraded := false
	for attempt := 0; ; attempt++ {
		identity, admin, err := connectOnce(ctx, client)
		if err == nil {
			if degraded {
				slog.Info("plex connection recovered; leaving degraded state")
			}
			return identity, admin, nil
		}
		if isFatalStartupError(err) {
			return nil, nil, err
		}
		if ctx.Err() != nil {
			return nil, nil, ctx.Err()
		}
		if degraded {
			slog.Warn("plex still unreachable; retrying", "error", err)
		} else {
			// First transient failure: mark healthy so the restart policy does
			// not crash-loop the container while Plex is unreachable, then keep
			// retrying. Recovery needs no counterpart flip — the marker is
			// already set and stays set.
			marker.Set(true)
			degraded = true
			slog.Warn("initial plex connection failed; starting in degraded state and retrying", "error", err)
		}
		select {
		case <-ctx.Done():
			return nil, nil, ctx.Err()
		case <-time.After(startupBackoff(attempt)):
		}
	}
}

// connectOnce performs a single connect + admin-resolve attempt.
func connectOnce(ctx context.Context, client *plex.Client) (*plex.ServerIdentity, *plex.User, error) {
	identity, err := client.ServerIdentity(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("connecting to plex server: %w", err)
	}
	admin, err := client.LoggedUser(ctx)
	if err != nil {
		return nil, nil, fmt.Errorf("resolving admin user: %w", err)
	}
	return identity, admin, nil
}

// startupBackoff returns the delay before retry attempt n (0-indexed):
// startupBaseBackoff * 2^n, capped at startupMaxBackoff. The shift is guarded
// so a large attempt count cannot overflow the duration to a negative value.
func startupBackoff(attempt int) time.Duration {
	if attempt < 0 || attempt >= 30 {
		return startupMaxBackoff
	}
	d := startupBaseBackoff << attempt
	if d <= 0 || d > startupMaxBackoff {
		return startupMaxBackoff
	}
	return d
}

// isFatalStartupError reports whether an initial Plex connect/admin-resolve
// error is a configuration or authentication problem that will not resolve
// without operator action (so run() should exit) rather than a transient
// connectivity failure (so run() should start degraded and keep retrying). A
// bad token (401/403) or other 4xx, a 404 (wrong server), and TLS/certificate
// errors are fatal; dial/DNS/timeout errors, 5xx (a Plex still starting up),
// and 429/408 (throttle/timeout signals) are treated as transient.
func isFatalStartupError(err error) bool {
	var statusErr *plex.HTTPStatusError
	if errors.As(err, &statusErr) {
		// 429 (Too Many Requests) and 408 (Request Timeout) are throttle/timeout
		// signals, not config/auth errors: treat them as transient so a busy or
		// slow Plex backs off and retries rather than exiting and crash-looping.
		if statusErr.Code == http.StatusTooManyRequests || statusErr.Code == http.StatusRequestTimeout {
			return false
		}
		return statusErr.Code < 500
	}
	// 404 on the identity endpoint: reached Plex, wrong server.
	if errors.Is(err, plex.ErrNotFound) {
		return true
	}
	// TLS/certificate misconfiguration (e.g. a self-signed cert without
	// PLEX_CA_CERT_PATH): will not recover without a config change.
	var certErr *tls.CertificateVerificationError
	if errors.As(err, &certErr) {
		return true
	}
	var unknownAuthority x509.UnknownAuthorityError
	if errors.As(err, &unknownAuthority) {
		return true
	}
	// Transport errors (connection refused, DNS failure, timeout): Plex is
	// unreachable now but may come back.
	return false
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

	userClient := n.users.ClientForUser(userID, n.client)
	if userClient == nil {
		slog.Warn("play event: no per-user client available, skipping",
			"user", username, "key", ev.RatingKey)
		return
	}
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
	if !n.cache.CheckAndMark(streamKey) {
		return
	}

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

		cacheKey := notify.BuildTimelineCacheKey(entry.ItemID)
		// Uses the WasRecentlyProcessed/MarkProcessed pair rather than the
		// atomic CheckAndMark on purpose: the key is marked (below) only after
		// the entry is confirmed a real, non-ignored episode, so an irrelevant
		// or ignored entry never suppresses a later genuine event for the same
		// ItemID (mark-on-success). CheckAndMark would mark-on-check and lose
		// that. Safe without atomicity here because timeline entries are
		// processed serially by the single listener goroutine; the atomic
		// CheckAndMark is reserved for the concurrent (scheduler pool) and
		// must-be-one-step (play streamKey) gates.
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
