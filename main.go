// Package main is the composition root for plex-language-sync.
//
// Responsibilities of this file:
//   - main() entry point + health subcommand dispatch.
//   - run(): construct the admin Plex client, cache, user manager,
//     syncer, and scheduler, wire them together, start the WebSocket
//     listener, and orchestrate a bounded shutdown join.
//   - notifyAdapter: the thin glue between internal/notify's WebSocket
//     listener and internal/tracksync. It gates play events on
//     cfg.triggerOnPlay and forwards relevant events to the syncer.
//
// Env-var contract, DEEP_SCAN_INTERVAL parsing, and _FILE-suffix secret
// handling live in config.go. Business logic lives under internal/.
package main

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sync"
	"syscall"
	"time"

	"github.com/cplieger/go/filesafe"
	"github.com/cplieger/go/health"
	"github.com/cplieger/go/httpserver"
	"github.com/cplieger/go/scheduler"
	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/plex-language-sync/internal/cache"
	"github.com/cplieger/plex-language-sync/internal/deepscan"
	"github.com/cplieger/plex-language-sync/internal/ignore"
	"github.com/cplieger/plex-language-sync/internal/notify"
	"github.com/cplieger/plex-language-sync/internal/obs"
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
// migrated on first load). Moving it strands every existing install's state.
const cacheDir = "/config"

// lastRunStampName is the deep-analysis last-run record (a scheduler.Stamp
// file) inside cacheDir, so it survives restarts on the same volume as the
// cache.
const lastRunStampName = ".plex-language-sync-last-run"

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
		health.RunProbe(health.DefaultPath, probeOptions()...)
	}
	os.Exit(run())
}

func run() int {
	ctx, cancel := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer cancel()

	cfg := loadConfig()
	logConfig(&cfg)

	metrics := obs.New()
	metrics.DeepScanInterval(cfg.schedulerInterval)
	stamp := scheduler.NewStamp(filepath.Join(cacheDir, lastRunStampName))
	seedDeepScan(metrics, stamp, &cfg)

	wsCfg := notify.DefaultConfig()
	live := newLiveness(health.DefaultPath, leasesFor(&cfg, wsCfg))
	wsCfg, deepScanLoop := live.wire(wsCfg)

	ln, err := bindMetrics(ctx, cfg.metricsListenAddr)
	if err != nil {
		slog.Error("cannot bind the metrics listener", "error", err)
		return 1
	}
	if ln != nil {
		done := serveMetrics(ctx, ln, metrics, live)
		defer func() { cancel(); <-done }()
		slog.Info("metrics listener started", "addr", ln.Addr().String())
	}

	tickerDone := make(chan struct{})
	go func() {
		defer close(tickerDone)
		live.run(ctx, livenessTick)
	}()
	defer func() { cancel(); <-tickerDone }()

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
	identity, admin, err := connectAndResolveAdmin(ctx, client, live)
	if err != nil {
		live.drain() // clear any degraded-healthy marker before exiting
		if ctx.Err() != nil {
			slog.Info("shutdown requested during startup", "cause", context.Cause(ctx))
			return 0
		}
		slog.Error("cannot establish initial plex connection", "error", err)
		return 1
	}
	logConnected(identity, admin)

	// Cache — load persistent state, or start fresh on error. The encryption
	// key (derived above) makes user-token decryption work offline on restart.
	c := cache.New()
	c.SetEncryptionKey(encKey)
	persist := &persister{cache: c, dir: cacheDir, metrics: metrics}
	if code, ok := checkCacheDir(ctx, live.drain); !ok {
		return code
	}
	if err := c.Load(cacheDir); err != nil {
		slog.Warn("cache load incomplete, affected sections starting fresh", "error", err)
	}
	// Reap any temp orphaned by an interrupted Save so they don't accumulate
	// on the persistent /config volume. Best-effort: a cleanup failure is
	// non-fatal at startup, so log at Warn and continue.
	if _, err := filesafe.CleanupStaleTemps(ctx, cacheDir, time.Hour); err != nil {
		slog.Warn("stale temp cleanup failed", "path", cacheDir, "error", err)
	}

	// User manager — admin identity + cached shared-user tokens.
	um := users.NewManager(c, metrics)
	um.Init(admin)
	um.LoadFromCache()

	// The app can serve. Starting before the plex.tv user refresh, ~75s of
	// retries on a plex.tv outage (DefaultRefreshConfig), keeps that outage
	// from failing a probe that a restart cannot fix.
	live.start()
	defer saveOnShutdown(live.drain, persist.save)

	// Synchronous initial refresh with bounded exponential backoff. See
	// internal/users/refresh.go for the retry semantics.
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
		syncerConfig(&cfg, ignorePolicy),
		tracksync.Deps{
			Cache:      c,
			Users:      um,
			UserClient: syncUserClient,
			Metrics:    metrics,
		},
	)
	sched := deepscan.New(
		deepscan.Config{
			Interval: cfg.schedulerInterval,
			Enable:   cfg.schedulerEnabled,
			Ignore:   ignorePolicy,
		},
		&deepscan.Deps{
			Plex:       client,
			Cache:      c,
			Stamp:      stamp,
			UserClient: scanUserClient,
			Sync:       syncer,
			SaveCache:  persist.save,
			Metrics:    metrics,
			Liveness:   deepScanLoop,
		},
	)

	// Join on RefreshLoop + deepscan.Run at shutdown so any in-flight work
	// completes before the deferred cache save writes its final snapshot.
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
	notify.NewListener(client, wsCfg).Listen(ctx, &notifyAdapter{
		syncer:        syncer,
		cfg:           &cfg,
		users:         um,
		client:        client,
		cache:         c,
		ignore:        ignorePolicy,
		resolveStalls: &resolveStallCounter{},
		metrics:       metrics,
	})

	slog.Info("shutting down", "cause", context.Cause(ctx))
	return 0
}

// seedDeepScan sets the last complete scan from stamp before /metrics serves,
// so no scrape after a restart reads 0; deepscan.Deps.Stamp owns what the
// stamp holds.
func seedDeepScan(m *obs.Metrics, stamp *scheduler.Stamp, cfg *config) {
	if !cfg.schedulerEnabled || cfg.schedulerInterval <= 0 {
		return
	}
	if rec, known := stamp.Last(); known {
		m.DeepScanSeeded(rec.Time)
	}
}

// bindMetrics binds the opt-in metrics listener before any work, so a bad
// address stops the container at once instead of leaving it running with no
// page. An empty addr binds nothing.
func bindMetrics(ctx context.Context, addr string) (net.Listener, error) {
	if addr == "" {
		return nil, nil
	}
	ln, err := (&net.ListenConfig{}).Listen(ctx, "tcp", addr)
	if err != nil {
		return nil, fmt.Errorf("METRICS_LISTEN_ADDR %q: %w", addr, err)
	}
	return ln, nil
}

// newListenerMux routes the opt-in listener: GET /metrics and GET /healthz.
func newListenerMux(m *obs.Metrics, live *liveness) *http.ServeMux {
	mux := http.NewServeMux()
	mux.Handle("GET /metrics", m.Handler())
	mux.Handle("GET /healthz", health.CheckHandler(live.checks()...))
	return mux
}

// serveMetrics serves newListenerMux on ln until ctx ends; done closes once
// the server has shut down.
func serveMetrics(ctx context.Context, ln net.Listener, m *obs.Metrics, live *liveness) (done <-chan struct{}) {
	srv := httpserver.New(newListenerMux(m, live), httpserver.WithSlogErrorLog(slog.LevelWarn))
	srv.ReadTimeout = 10 * time.Second
	srv.WriteTimeout = 30 * time.Second
	stopped := make(chan struct{})
	go func() {
		defer close(stopped)
		if err := httpserver.Run(ctx, srv, ln); err != nil {
			slog.Warn("metrics listener stopped", "error", err)
		}
	}()
	return stopped
}

// persister is the runtime save path, used by the deep scan and at shutdown,
// so each of those failed saves is counted.
type persister struct {
	cache   *cache.Cache
	metrics *obs.Metrics
	dir     string
}

func (p *persister) save() error {
	err := p.cache.Save(p.dir)
	if err != nil {
		p.metrics.CacheSaveFailed()
	}
	return err
}

// saveOnShutdown drains liveness, so Docker stops passing its probe while the
// slow save runs, then saves. A failed save here loses the profiles and tokens
// learned since the last one, so it is an ERROR, where a mid-run failure the
// next pass retries is a WARN.
func saveOnShutdown(drain func(), save func() error) {
	drain()
	if err := save(); err != nil {
		slog.Error("cache save on shutdown failed, profiles may be lost",
			"path", cacheDir, "error", err)
	}
}

// syncerConfig is the part of cfg the syncer reads. TRIGGER_ON_SCAN becomes
// SeedNewEpisodes, the one switch for every path that seeds new episodes.
func syncerConfig(cfg *config, policy *ignore.Policy) tracksync.Config {
	return tracksync.Config{
		UpdateLevel:      cfg.updateLevel,
		UpdateStrategy:   cfg.updateStrategy,
		Ignore:           policy,
		SubtitleFloor:    cfg.subtitleTier,
		LanguageProfiles: cfg.languageProfiles,
		SeedNewEpisodes:  cfg.triggerOnScan,
	}
}

// checkCacheDir runs the startup writability check on the cache directory and
// reports whether startup may continue; when it may not, it calls drain and
// code is the exit code.
func checkCacheDir(ctx context.Context, drain func()) (code int, ok bool) {
	err := cache.CheckWritable(ctx, cacheDir)
	if err == nil {
		return 0, true
	}
	drain()
	if ctx.Err() != nil {
		slog.Info("shutdown requested during startup", "cause", context.Cause(ctx))
		return 0, false
	}
	logCacheUnwritable(cacheDir, err)
	return 1, false
}

// logCacheUnwritable names the remedy for a failed startup writability check.
// A mode the filesystem would not store is an ACL or mount-option problem,
// which ownership changes cannot fix.
func logCacheUnwritable(dir string, err error) {
	if errors.Is(err, filesafe.ErrModeNotStored) {
		slog.Error("cache directory cannot hold owner-only files; remove the inherited ACL or mount option that widens new files, since the cache keeps user tokens",
			"path", dir, "error", err)
		return
	}
	slog.Error("cache directory is not writable; give the container user write access to it",
		"path", dir, "error", err)
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
// first transient failure it starts live (a degraded start), then keeps
// retrying until Plex answers, beating live's reconnect entry on each attempt
// and parking it on return. A fatal error (bad token / 4xx, a wrong-server
// 404, or a TLS/cert misconfiguration) returns immediately so the caller can
// exit non-zero. Returns ctx.Err() when shutdown is signalled mid-retry.
func connectAndResolveAdmin(ctx context.Context, client *plex.Client, live *liveness) (*plex.ServerIdentity, *plex.User, error) {
	defer live.reconnect.Park()
	degraded := false
	for attempt := 0; ; attempt++ {
		live.reconnect.Beat()
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
			// First transient failure: start healthy so the restart policy does
			// not crash-loop the container while Plex is unreachable, then keep
			// retrying.
			live.start()
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

// logConnected records the server identity and admin account read from
// Plex at startup.
func logConnected(identity *plex.ServerIdentity, admin *plex.User) {
	slog.Info("connected to plex server",
		"name", textsafe.SanitizeSingleLine(identity.FriendlyName),
		"id", textsafe.SanitizeSingleLine(identity.MachineIdentifier),
		"version", textsafe.SanitizeSingleLine(identity.Version))
	slog.Info("authenticated as admin user",
		"name", textsafe.SanitizeSingleLine(admin.Name),
		"id", textsafe.SanitizeSingleLine(admin.ID))
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
// Plex status the library's IsTransient does not report (a bad token or other
// 4xx), a 404 (wrong server), and TLS/certificate errors are fatal;
// dial/DNS/timeout errors and transient statuses are not.
func isFatalStartupError(err error) bool {
	if statusErr, ok := errors.AsType[*plex.HTTPStatusError](err); ok {
		return !statusErr.IsTransient()
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

type episodeSkipper interface {
	ShouldSkipEpisode(ctx context.Context, ref *streams.Episode) bool
}

// notifyAdapter joins the notify listener to the syncer. It gates play
// events on cfg.triggerOnPlay; the syncer owns the seeding switch that
// timeline events follow. The handlers live here, not in tracksync, because
// the events are notify types and the ignore and dedup rules mix cache state
// with config.
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
	metrics       *obs.Metrics
}

func (n *notifyAdapter) OnPlay(ctx context.Context, ev notify.PlayEvent) {
	if !n.cfg.triggerOnPlay {
		return
	}
	n.handlePlayEvent(ctx, ev)
}

func (n *notifyAdapter) OnTimeline(ctx context.Context, entries []notify.TimelineEntry) {
	n.handleTimeline(ctx, entries)
}

// OnConnection records the stream state; every drop leads to one reconnect.
func (n *notifyAdapter) OnConnection(_ context.Context, connected bool) {
	n.metrics.WebSocketConnected(connected)
	if !connected {
		n.metrics.WebSocketReconnect()
	}
}

// handlePlayEvent processes a single play session state notification.
func (n *notifyAdapter) handlePlayEvent(ctx context.Context, ev notify.PlayEvent) {
	if !notify.IsRelevantPlayEvent(ev) {
		return
	}

	userID, username, ok := n.resolvePlayEventUser(ctx, ev)
	n.metrics.PlayEvent(ok)
	if !ok {
		return
	}

	userClient := n.users.ClientForUser(userID, n.client)
	if userClient == nil {
		slog.Warn("play event: no per-user client available, skipping",
			"user", username, "key", textsafe.SanitizeSingleLine(ev.RatingKey))
		return
	}
	episode, err := userClient.Episode(ctx, plex.RatingKey(ev.RatingKey))
	if err != nil {
		if !errors.Is(err, plex.ErrNotFound) {
			slog.Debug("play event: failed to fetch episode",
				"key", textsafe.SanitizeSingleLine(ev.RatingKey), "user", username, "error", err)
		}
		return
	}
	if episode.Type != plex.TypeEpisode {
		return
	}

	// Observe's only error is the context's; Plex re-announces the play.
	if err := n.syncer.Observe(ctx, userID, episode, "play"); err != nil {
		slog.Debug("play event: stopped before the choice was applied",
			"key", textsafe.SanitizeSingleLine(ev.RatingKey), "user", username, "error", err)
	}
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
		n.metrics.ResolutionRecovered()
	}
	// The account holder chooses the name and callers only log it.
	return uid, textsafe.SanitizeSingleLine(uname), true
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
		"client", textsafe.SanitizeSingleLine(ev.ClientIdentifier),
		"key", textsafe.SanitizeSingleLine(ev.RatingKey),
		"state", textsafe.SanitizeSingleLine(ev.State),
		"error", textsafe.SanitizeSingleLine(cause.Error()))

	// A skip is either an ABSENCE (the session list was read and did not
	// carry this client, or the notification named no client to look up)
	// or an UNREADABLE session list. Only the second is evidence about
	// resolution itself.
	absent := errors.Is(cause, errUnattributedNoClient) ||
		errors.Is(cause, plex.ErrNoSessionForClient)

	if v := n.resolveStalls.miss(ev, !absent); v.stalled {
		slog.Warn("play event: user resolution stalled; no playback has been attributed for a sustained run",
			"cause", string(v.cause),
			"consecutive_failures", v.consecutive,
			"absent_pairs", v.absentPairs,
			"last_client", textsafe.SanitizeSingleLine(ev.ClientIdentifier),
			"last_key", textsafe.SanitizeSingleLine(ev.RatingKey))
		n.metrics.ResolutionStalled(string(v.cause))
	}
}

// resolveStallThreshold is the number of consecutive unattributed play
// events that means the session list itself has stopped answering.
//
// Calibrated against 30 days of production traffic: 2814 resolution
// attempts, 2651 attributed and 163 skipped, and the longest run of
// consecutive skips with no success between them was 12. Twenty is
// 1.7x that observed maximum, so the expected races never reach it,
// while a resolver that answers nothing crosses it in about three
// minutes of playback at Plex's ~10s notification cadence.
const resolveStallThreshold = 20

// resolveAbsentPairThreshold is the number of DISTINCT (client, item)
// pairs absent from a readable session list that means the join is
// failing rather than a client misbehaving.
//
// Distinct pairs rather than event count, because run length measures
// how noisy one stale announcer is: a zombie or an idle tab repeats one
// pair forever, while a failing join produces a new pair for every
// client and item anyone touches. Five absorbs a household's worth of
// simultaneously stale devices (three were live during the 2026-09-14
// false alert) and is reached quickly by a real fault, because any
// single success clears the run.
const resolveAbsentPairThreshold = 5

// stallCause names why a run of unattributed play events is reportable.
// It rides on the Warn line so the operator is told which of the two
// states to go and look at, because they have different remedies.
type stallCause string

const (
	// causeSessionsUnreadable: the active-session list itself could not
	// be read, so no client can be attributed whatever it is doing. The
	// operator-actionable state, and the one the published alert's
	// advice fits: a token that lost its rights, or Plex not answering.
	causeSessionsUnreadable stallCause = obs.StallSessionsUnreadable

	// causeAllClientsAbsent: the list was read every time and several
	// distinct (client, item) pairs in the run were missing from it. A
	// readable list that carries none of them is the join failing, while
	// one pair missing repeatedly is that one client or that one item.
	causeAllClientsAbsent stallCause = obs.StallAllClientsAbsent
)

// resolveStallCounter tracks a run of unattributed play events so a
// stall is reported once instead of once per notification, and decides
// which runs are worth reporting at all.
//
// Neither a count nor a client spread is evidence on its own, and both
// produced a false alert. On 2026-08-30 Plex removed a WAN client's
// session mid-film, the client kept announcing the same paused item
// every 20s with nobody else watching, and 20 skips accumulated in
// under 10 minutes. On 2026-09-14 the same thing happened to a paused
// Android session alongside a second idle client, which satisfied a
// rule requiring more than one client. Both times /status/sessions
// answered correctly and the token was fine, which is everything the
// alert tells the operator to check.
//
// So a run escalates on one of two grounds, never on length:
//
//   - the session list could not be READ, sustained across
//     resolveStallThreshold. Nothing can be attributed while that
//     holds, so this arm needs no spread and closes the single-viewer
//     case a spread rule would miss. A paused event still counts here,
//     because an unreadable list is unreadable whatever is playing.
//   - resolveAbsentPairThreshold DISTINCT (client, item) pairs were
//     absent from a list that read fine. A stale announcer is one pair
//     however long it repeats; a failing join produces a new pair per
//     client and item. Paused announcements are excluded, because a
//     paused client has no new selection to attribute.
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
	// absentPairs holds the distinct (client, item) pairs missing from a
	// readable session list. A set, so one client repeating one item
	// contributes once however many notifications it sends.
	absentPairs map[absentPair]struct{}
	// mu guards every field of this struct.
	mu     sync.Mutex
	misses int
	// unreadableRun is the tail of the run that could not read the
	// session list. Reset by any readable miss, so a single transient
	// read failure inside an otherwise-benign run cannot escalate it.
	unreadableRun int
	warned        bool
}

// absentPair identifies one thing that could not be attributed. A struct
// key rather than a joined string, so no client identifier containing the
// separator can forge another pair's identity.
type absentPair struct {
	client    string
	ratingKey string
}

// stallVerdict is miss's answer: whether this event makes the run
// reportable, the ground for reporting it, and the two counts the log
// line carries so an operator can tell the arms apart.
type stallVerdict struct {
	cause       stallCause
	consecutive int
	absentPairs int
	stalled     bool
}

// miss records an unattributed event and reports whether this is the one
// that makes the run reportable. Stalled is true at most once per run.
func (c *resolveStallCounter) miss(ev notify.PlayEvent, sessionsUnreadable bool) stallVerdict {
	if c == nil {
		return stallVerdict{}
	}
	c.mu.Lock()
	defer c.mu.Unlock()

	c.misses++
	switch {
	case sessionsUnreadable:
		c.unreadableRun++
	case notify.IsPausedPlayEvent(ev):
		c.unreadableRun = 0
	default:
		c.unreadableRun = 0
		if c.absentPairs == nil {
			c.absentPairs = make(map[absentPair]struct{})
		}
		c.absentPairs[absentPair{client: ev.ClientIdentifier, ratingKey: ev.RatingKey}] = struct{}{}
	}

	v := stallVerdict{consecutive: c.misses, absentPairs: len(c.absentPairs)}
	if c.warned {
		return v
	}
	switch {
	case c.unreadableRun >= resolveStallThreshold:
		c.warned, v.stalled, v.cause = true, true, causeSessionsUnreadable
	case len(c.absentPairs) >= resolveAbsentPairThreshold:
		c.warned, v.stalled, v.cause = true, true, causeAllClientsAbsent
	}
	return v
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
	clear(c.absentPairs)
	c.warned = false
	return recovered, after
}

func (n *notifyAdapter) handleTimeline(ctx context.Context, entries []notify.TimelineEntry) {
	if !n.syncer.SeedsNewEpisodes() {
		return
	}
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
				"id", textsafe.SanitizeSingleLine(entry.ItemID), "error", err)
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

		n.syncer.SeedEpisode(ctx, episode, action)
	}
}
