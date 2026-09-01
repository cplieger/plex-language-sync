// Package main is the composition root for plex-language-sync.
//
// Responsibilities of this file:
//   - main() entry point + health subcommand dispatch.
//   - run(): construct the admin Plex client, cache, user manager,
//     syncer, and scheduler, wire them together, start the WebSocket
//     listener, and orchestrate a bounded shutdown join.
//   - notifyAdapter: the thin glue between internal/notify's WebSocket
//     listener and internal/tracksync. It gates on cfg.triggerOnPlay /
//     cfg.triggerOnScan and forwards relevant events to the syncer.
//
// Env-var contract, DEEP_SCAN_INTERVAL parsing, and _FILE-suffix secret
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
	"sync"
	"syscall"
	"time"

	"github.com/cplieger/atomicfile/v3"
	"github.com/cplieger/health"
	"github.com/cplieger/plex-language-sync/internal/cache"
	"github.com/cplieger/plex-language-sync/internal/deepscan"
	"github.com/cplieger/plex-language-sync/internal/ignore"
	"github.com/cplieger/plex-language-sync/internal/notify"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plex-language-sync/internal/tracksync"
	"github.com/cplieger/plex-language-sync/internal/users"
)

// Compile-time assertion that the real client satisfies the per-user surface
// tracksync declares. Checked here, at the composition root, because that is
// where the two are joined — neither package imports the other.
var _ tracksync.PlexReadWriter = (*plex.Client)(nil)

// cacheDir is the on-disk directory for the persisted cache (the split
// profiles.json / tokens.json / state.json layout; a legacy cache.json is
// migrated on first load). Frozen by inviolate contract item 7 (file paths).
const cacheDir = "/config"

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

	// NewClient warns (via the shared library) when the URL is plain http
	// to a non-local host — the X-Plex-Token would transit unencrypted.
	client, err := plex.NewClient(plex.Options{ServerURL: cfg.plexURL, Token: plex.Token(cfg.plexToken), CACertPath: cfg.caCertPath})
	if err != nil {
		slog.Error("cannot initialize plex client", "error", err)
		return 1
	}

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
	if err := c.Load(cacheDir); err != nil {
		slog.Warn("cache load incomplete, affected sections starting fresh", "error", err)
	}
	// Reap any temp orphaned by an interrupted Save so they don't accumulate
	// on the persistent /config volume. Best-effort: a cleanup failure is
	// non-fatal at startup, so log at Warn and continue.
	if _, err := atomicfile.CleanupStaleTemps(ctx, cacheDir, time.Hour); err != nil {
		slog.Warn("stale temp cleanup failed", "path", cacheDir, "error", err)
	}

	// User manager — admin identity + cached shared-user tokens.
	um := users.NewManager(c)
	um.Init(admin)
	um.LoadFromCache()

	// Connection verified and admin resolved, cache + user manager
	// initialized: the app can serve. marker.Set(true) is idempotent — the
	// connect loop already set it if the initial connection was degraded;
	// setting it here also covers the connected-on-first-try path. Marked
	// BEFORE the plex.tv shared-user refresh so container liveness is not
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
		if err := c.Save(cacheDir); err != nil {
			slog.Error("cache save on shutdown failed, profiles may be lost",
				"path", cacheDir, "error", err)
		}
	}()

	// Synchronous initial refresh with bounded exponential backoff. See
	// internal/users/refresh.go for the retry semantics. Runs after the
	// health marker is set so a plex.tv outage never gates liveness.
	um.InitialRefreshWithRetry(ctx, client, identity.MachineIdentifier, users.DefaultRefreshConfig())

	// Compose the subsystems from the concrete internal/* packages. Each
	// subsystem declares its own narrow interfaces, so what it can reach is
	// bounded by what it asked for rather than by a shared contract package.
	//
	// ClientForUser returns a typed nil (*plex.Client)(nil) when no per-user
	// client can be built, and putting that straight into an interface yields a
	// NON-nil interface wrapping a nil pointer (the Go nil-interface trap),
	// which would defeat every consumer's `== nil` check. perUserClient is the
	// single place that conversion happens; the two adapters below only
	// narrow its result.
	perUserClient := func(userID string) *plex.Client {
		return um.ClientForUser(userID, client)
	}
	syncUserClient := func(userID string) tracksync.PlexReadWriter {
		if uc := perUserClient(userID); uc != nil {
			return uc
		}
		return nil
	}
	scanUserClient := func(userID string) deepscan.EpisodeReader {
		if uc := perUserClient(userID); uc != nil {
			return uc
		}
		return nil
	}
	ignorePolicy := ignore.New(ignore.Config{
		Reader:    client,
		Libraries: cfg.ignoreLibraries,
		Labels:    cfg.ignoreLabels,
	})
	syncer := tracksync.New(
		tracksync.Config{
			UpdateLevel:      cfg.updateLevel,
			UpdateStrategy:   cfg.updateStrategy,
			Ignore:           ignorePolicy,
			SubtitleFloor:    cfg.subtitleTier,
			LanguageProfiles: cfg.languageProfiles,
		},
		tracksync.Deps{
			Plex:       client,
			Cache:      c,
			Users:      um,
			UserClient: syncUserClient,
		},
	)
	sched := deepscan.New(
		deepscan.Config{
			Interval: cfg.schedulerInterval,
			Enable:   cfg.schedulerEnabled,
			Ignore:   ignorePolicy,
		},
		deepscan.Deps{
			Plex:       client,
			Cache:      c,
			UserClient: scanUserClient,
			Sync:       syncer,
			SaveCache:  func() error { return c.Save(cacheDir) },
		},
	)

	// runtime-concurrency-p2: join on RefreshLoop + deepscan.Run at
	// shutdown so any in-flight work completes before the deferred cache
	// save writes its final snapshot.
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
	notify.NewListener(client, notify.DefaultConfig()).Listen(ctx, &notifyAdapter{
		syncer:        syncer,
		cfg:           &cfg,
		users:         um,
		client:        client,
		cache:         c,
		ignore:        ignorePolicy,
		resolveStalls: &resolveStallCounter{},
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
	identity, err := client.Identity(ctx)
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
	if statusErr, ok := errors.AsType[*plex.HTTPStatusError](err); ok {
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
	if _, ok := errors.AsType[*tls.CertificateVerificationError](err); ok {
		return true
	}
	if _, ok := errors.AsType[x509.UnknownAuthorityError](err); ok {
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
// listener (internal/notify) and the sync subsystem (internal/tracksync).
// It gates play events on cfg.triggerOnPlay and timeline events on
// cfg.triggerOnScan, then forwards relevant events to the sync
// subsystem. The per-event handlers live here (not in internal/tracksync)
// because the event shape is notify-package-typed and the
// ignore/dedup rules are a blend of cache state and config — both of
// which are main-package concerns.
//
// Methods take a POINTER receiver: every field is a handle onto shared
// mutable state (the cache, the users manager, the stall counter), so a
// value receiver would copy the struct on every event for no reason.
type episodeSkipper interface {
	ShouldSkipEpisode(ctx context.Context, ref *streams.Episode) bool
}

type notifyAdapter struct {
	syncer *tracksync.Syncer
	cfg    *config
	users  *users.Manager
	client *plex.Client
	cache  *cache.Cache
	ignore episodeSkipper
	// resolveStalls carries the identity-resolution failure run across
	// events so the expected failures stay quiet and a real stall speaks
	// once. A nil counter counts nothing (see resolveStallCounter).
	resolveStalls *resolveStallCounter
}

func (n *notifyAdapter) OnPlay(ctx context.Context, ev notify.PlayEvent) {
	if !n.cfg.triggerOnPlay {
		return
	}
	n.handlePlayEvent(ctx, ev)
}

func (n *notifyAdapter) OnTimeline(ctx context.Context, entries []notify.TimelineEntry) {
	if !n.cfg.triggerOnScan {
		return
	}
	n.handleTimeline(ctx, entries)
}

// handlePlayEvent processes a single play session state notification.
func (n *notifyAdapter) handlePlayEvent(ctx context.Context, ev notify.PlayEvent) {
	if !notify.IsRelevantPlayEvent(ev) {
		return
	}

	userID, username, ok := n.resolvePlayEventUser(ctx, ev)
	if !ok {
		return
	}

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

	cur := streams.Selected(episode)
	streamKey := notify.BuildStreamCacheKey(notify.StreamSelectionKey{
		UserID:     userID,
		RatingKey:  ev.RatingKey,
		AudioID:    streams.ID(cur.Audio),
		SubtitleID: streams.ID(cur.Subtitle),
	})
	if !n.cache.CheckAndMark(streamKey) {
		return
	}

	slog.Info("play event detected",
		"episode", episode.ShortName(),
		"user", username,
		"state", ev.State)

	n.syncer.ObserveAndPropagate(ctx, userClient, userID, episode, "play")
}

// resolvePlayEventUser resolves the user from a play event's client
// identifier. Fails CLOSED: when the event carries no client identifier
// or the session cannot be resolved, it returns ok=false and the caller
// skips the event rather than misattributing it to the admin — a
// per-user stream write under the wrong identity records the selection
// against the wrong user, and a mis-learned profile poisons future
// seeding.
//
// A skip is terminal for that notification: the reconcile plane does
// NOT recover it, because replay re-applies RECORDED intents and a
// skipped event recorded none. What recovers it is the notification
// stream itself: Plex re-announces an active session about every 10s,
// so a genuine session that was merely not yet queryable is attributed
// on a later notification.
func (n *notifyAdapter) resolvePlayEventUser(ctx context.Context, ev notify.PlayEvent) (userID, username string, ok bool) {
	if ev.ClientIdentifier == "" {
		n.skipUnattributedPlayEvent(ev, errUnattributedNoClient)
		return "", "", false
	}
	uid, uname, err := n.client.UserFromSession(ctx, ev.ClientIdentifier)
	if err != nil {
		n.skipUnattributedPlayEvent(ev, err)
		return "", "", false
	}
	if recovered, after := n.resolveStalls.success(); recovered {
		slog.Info("play event: user resolution recovered",
			"after_consecutive_failures", after)
	}
	return uid, uname, true
}

// errUnattributedNoClient is the cause recorded when Plex sends a play
// notification with no clientIdentifier at all, leaving nothing to join
// against /status/sessions.
var errUnattributedNoClient = errors.New("event carries no client identifier")

// skipUnattributedPlayEvent records one fail-closed skip and escalates
// only when the run of them says resolution has stopped working.
//
// Deliberately Debug, not Warn: an unattributed play event is an
// EXPECTED, self-healing outcome. Measured 2026-08-15:
//
//   - Plex announces playback before the session is queryable. A client
//     was announced at 08:02:20, 08:02:31 and 08:02:41 while
//     /status/sessions first carried the session at 08:02:51, which is
//     also when this app first attributed it. The next notification
//     arrives ~10s later and carries the real one.
//   - An idle Plex Web client keeps re-announcing its last item long
//     after the session ends: one browser client produced 19 skips for
//     a single ratingKey spanning 73 hours, every one outside all four
//     of that client's real session windows.
//
// Neither is actionable by an operator, and at Warn they buried the one
// state that is: resolution failing across the board. That is what the
// run counter reports, once per stall rather than once per notification
// — see resolveStallCounter for how it tells the two shapes apart.
func (n *notifyAdapter) skipUnattributedPlayEvent(ev notify.PlayEvent, cause error) {
	slog.Debug("play event: skipping, user not attributed",
		"client", ev.ClientIdentifier, "key", ev.RatingKey,
		"state", ev.State, "error", cause)

	// A skip is either an ABSENCE (the session list was read and did not
	// carry this client, or the notification named no client to look up)
	// or an UNREADABLE session list. Only the second is evidence about
	// resolution itself.
	absent := errors.Is(cause, errUnattributedNoClient) ||
		errors.Is(cause, plex.ErrNoSessionForClient)

	if stalled, consecutive, why := n.resolveStalls.miss(ev.ClientIdentifier, !absent); stalled {
		slog.Warn("play event: user resolution stalled; no playback has been attributed for a sustained run",
			"cause", string(why),
			"consecutive_failures", consecutive,
			"last_client", ev.ClientIdentifier, "last_key", ev.RatingKey)
	}
}

// resolveStallThreshold is the number of consecutive unattributed play
// events that means resolution is broken rather than racing.
//
// Calibrated against 30 days of production traffic: 2814 resolution
// attempts, 2651 attributed and 163 skipped, and the longest run of
// consecutive skips with no success between them was 12. Twenty is
// 1.7x that observed maximum, so the expected races never reach it,
// while a resolver that answers nothing crosses it in about three
// minutes of playback at Plex's ~10s notification cadence.
const resolveStallThreshold = 20

// stallCause names why a run of unattributed play events is reportable.
// It rides on the Warn line so the operator is told which of the two
// states to go and look at, because they have different remedies.
type stallCause string

const (
	// causeSessionsUnreadable: the active-session list itself could not
	// be read, so no client can be attributed whatever it is doing. The
	// operator-actionable state, and the one the published alert's
	// advice fits: a token that lost its rights, or Plex not answering.
	causeSessionsUnreadable stallCause = "sessions_unreadable"

	// causeAllClientsAbsent: the list was read every time and no client
	// in the run appeared in it. Reportable only across more than one
	// client, because a readable list that never carries ANY client is a
	// real fault, while one client missing from it is that client.
	causeAllClientsAbsent stallCause = "all_clients_absent"
)

// resolveStallCounter counts consecutive unattributed play events so a
// stall is reported once instead of once per notification, and decides
// which runs are worth reporting at all.
//
// A count on its own is not evidence that resolution is broken: on
// 2026-08-30, Plex removed a WAN client's session mid-film ("Client
// stopped playback"), the client kept announcing the same paused
// ratingKey every 20s, and 20 skips accumulated in under 10 minutes
// with nobody else watching, so no success arrived to clear the run.
// /status/sessions answered correctly throughout and the token was
// fine — everything the alert would have told the operator to check.
//
// So a run escalates on one of two grounds, never on length alone:
//
//   - the session list could not be READ, sustained across the whole
//     threshold. Nothing can be attributed while that holds, so this
//     arm needs no client spread and closes the single-viewer case a
//     spread rule would miss.
//   - the list was read and NO client in the run was in it, across more
//     than one client. One client absent from a readable list is that
//     client (a start race, a stale tab, a removed session); every
//     client absent is the join failing.
//
// Guarded by a mutex rather than left to the listener's serial dispatch:
// notify.Handler explicitly permits a handler to hand work to a
// goroutine, so serial dispatch is today's implementation detail and not
// a contract this type should depend on.
//
// A nil counter counts nothing and never escalates, which lets
// resolvePlayEventUser be exercised by tests that build no composition
// root; production wiring always supplies one.
type resolveStallCounter struct {
	// client is the first client identifier in the run and clientSeen
	// distinguishes "not set yet" from an event that named no client,
	// which is itself a valid (empty) identifier.
	client string
	// mu guards every field of this struct.
	mu     sync.Mutex
	misses int
	// unreadableRun is the tail of the run that could not read the
	// session list. Reset by any readable miss, so a single transient
	// read failure inside an otherwise-benign run cannot escalate it.
	unreadableRun int
	clientSeen    bool
	multiClient   bool
	warned        bool
}

// miss records an unattributed event and reports whether this is the one
// that makes the run reportable, with the ground for reporting it. True
// at most once per run.
func (c *resolveStallCounter) miss(client string, sessionsUnreadable bool) (stalled bool, consecutive int, cause stallCause) {
	if c == nil {
		return false, 0, ""
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.misses++
	if sessionsUnreadable {
		c.unreadableRun++
	} else {
		c.unreadableRun = 0
	}
	switch {
	case !c.clientSeen:
		c.client, c.clientSeen = client, true
	case client != c.client:
		c.multiClient = true
	}

	if c.warned {
		return false, c.misses, ""
	}
	switch {
	case c.unreadableRun >= resolveStallThreshold:
		c.warned = true
		return true, c.misses, causeSessionsUnreadable
	case c.multiClient && c.misses >= resolveStallThreshold:
		c.warned = true
		return true, c.misses, causeAllClientsAbsent
	}
	return false, c.misses, ""
}

// success clears the run and reports whether it ended a stall that was
// warned about, so recovery is a positive log line rather than the
// absence of one.
func (c *resolveStallCounter) success() (recovered bool, after int) {
	if c == nil {
		return false, 0
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	after, recovered = c.misses, c.warned
	c.misses, c.unreadableRun = 0, 0
	c.client, c.clientSeen, c.multiClient = "", false, false
	c.warned = false
	return recovered, after
}

func (n *notifyAdapter) handleTimeline(ctx context.Context, entries []notify.TimelineEntry) {
	for i := range entries {
		entry := &entries[i]
		if !notify.IsRelevantTimelineEntry(entry) {
			continue
		}

		cacheKey := notify.BuildTimelineCacheKey(entry.ItemID)
		// Uses the WasRecentlyProcessed/MarkProcessed pair rather than the
		// atomic CheckAndMark: the key is marked (below) only after the
		// entry is confirmed a real, non-ignored episode, so an irrelevant
		// or ignored entry never suppresses a later genuine event for the
		// same ItemID. Safe without atomicity here because timeline entries
		// are processed serially by the single listener goroutine.
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
		if n.ignore != nil && n.ignore.ShouldSkipEpisode(ctx, episode) {
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
