package main

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cplieger/go/health/v2"
	"github.com/cplieger/plex-language-sync/internal/notify"
)

// livenessTick is how often the ticker reads the loops and sets the verdict.
const livenessTick = 10 * time.Second

// markerLease is how old the last healthy verdict may get, for the health
// subcommand and /healthz alike: three ticks, so a late tick passes and only a
// stopped ticker fails it.
var markerLease = health.Lease{Interval: livenessTick, Cycles: 3}

// healthMarkerPath is the file the verdict is mirrored to for the health
// subcommand.
var healthMarkerPath = health.MarkerPath("plex-language-sync")

// deepScanPassAllowance is the slack the deep-scan lease adds to two
// intervals, the same 2h the deep scan alerts add; nothing bounds a pass.
const deepScanPassAllowance = 2 * time.Hour

// connectAttemptBudget is the longest one connect attempt may take: the
// startup attempt makes two Plex requests, each bounded by plexapi's
// two-minute request timeout.
const connectAttemptBudget = 5 * time.Minute

// runHealthProbe is the health subcommand: it exits 0 while the marker at path
// is no older than markerLease, or cannot be written, and 1 otherwise.
func runHealthProbe(path string) {
	health.RunProbe(path, health.WithMaxAge(markerLease.Duration()))
}

// loopLeases holds each loop's lease; a zero lease is not checked.
type loopLeases struct {
	websocket, reconnect, deepScan time.Duration
}

// leasesFor derives each loop's lease from the settings and the WebSocket
// listener's config.
func leasesFor(cfg *config, ws notify.Config) loopLeases {
	ls := loopLeases{
		// One read deadline, and one more for the handler of the last message.
		websocket: health.Lease{Interval: ws.ReadIdleTimeout, Cycles: 2}.Duration(),
		reconnect: health.Lease{
			Interval: max(ws.MaxBackoff, startupMaxBackoff), Cycles: 1,
			Timeout: connectAttemptBudget, Attempts: 1,
		}.Duration(),
	}
	if cfg.schedulerEnabled && cfg.schedulerInterval > 0 {
		ls.deepScan = health.Lease{
			Interval: cfg.schedulerInterval, Cycles: 2,
			Timeout: deepScanPassAllowance, Attempts: 1,
		}.Duration()
	}
	return ls
}

// loopEntry is one loop's liveness entry, and a health.Signal: healthy while
// parked, while its lease is 0, or while its last beat is no older than the
// lease, the comparison the probe's deadline makes.
type loopEntry struct {
	last time.Time
	name string
	// reason is the fixed sentence /healthz lists while the entry is stale.
	reason string
	lease  time.Duration
	mu     sync.Mutex
	parked bool
}

var (
	_ notify.Loop   = (*loopEntry)(nil)
	_ health.Signal = (*loopEntry)(nil)
)

func newLoopEntry(name, reason string, lease time.Duration) *loopEntry {
	return &loopEntry{name: name, reason: reason, lease: lease, parked: true}
}

// Beat records progress and arms the entry.
func (e *loopEntry) Beat() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.last = time.Now()
	e.parked = false
}

// Park marks the loop idle by design, so its silence is no fault until the
// next Beat.
func (e *loopEntry) Park() {
	e.mu.Lock()
	defer e.mu.Unlock()
	e.parked = true
}

// Healthy reports whether the loop is inside its lease.
func (e *loopEntry) Healthy() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.parked || e.lease <= 0 || time.Since(e.last) <= e.lease
}

// liveness is the one owner of the health verdict. Each loop beats its own
// entry and one ticker sets the verdict from all of them, so a wedged loop
// turns the container unhealthy while the others keep working.
type liveness struct {
	status    *health.Status
	websocket *loopEntry
	reconnect *loopEntry
	deepScan  *loopEntry
	// stale holds the loops already reported stale, so each stall logs once.
	stale map[*loopEntry]bool
	mu    sync.Mutex
	// serving is atomic so /healthz never waits on l.mu, which a marker write
	// holds.
	serving atomic.Bool
}

// newLiveness returns a liveness mirroring its verdict to the marker at path,
// unhealthy, with every entry parked and the app not yet serving. Build it in
// the long-running process only: it removes the marker another process left.
func newLiveness(path string, leases loopLeases) *liveness {
	return &liveness{
		status:    health.NewStatus(health.WithMarker(path)),
		websocket: newLoopEntry("websocket", "the Plex WebSocket listener stopped making progress", leases.websocket),
		reconnect: newLoopEntry("reconnect", "the Plex reconnect loop stopped making progress", leases.reconnect),
		deepScan:  newLoopEntry("deep_scan", "the deep scan stopped making progress", leases.deepScan),
		stale:     map[*loopEntry]bool{},
	}
}

func (l *liveness) loops() []*loopEntry {
	return []*loopEntry{l.websocket, l.reconnect, l.deepScan}
}

// wire returns ws with the WebSocket listener's two loops beating their
// entries, and the entry the deep scan beats.
func (l *liveness) wire(ws notify.Config) (notify.Config, *loopEntry) {
	ws.ReadLoop, ws.ReconnectLoop = l.websocket, l.reconnect
	return ws, l.deepScan
}

// verdictRefreshed fails once the ticker stops setting the verdict while
// every other check passes; when one fails, that check names the cause.
func (l *liveness) verdictRefreshed() bool {
	if !l.serving.Load() {
		return true
	}
	for _, e := range l.loops() {
		if !e.Healthy() {
			return true
		}
	}
	return l.status.Healthy() && l.status.Fresh(markerLease.Duration()).Healthy()
}

// checks are /healthz's: one per loop, plus the two other conditions the
// probe fails on, the app not serving and a verdict the ticker no longer
// refreshes, so the endpoint and the probe give one verdict.
func (l *liveness) checks() []health.Check {
	cs := []health.Check{{Signal: health.SignalFunc(l.serving.Load), Reason: "the app is starting or shutting down"}}
	for _, e := range l.loops() {
		cs = append(cs, health.Check{Signal: e, Reason: e.reason})
	}
	return append(cs, health.Check{Signal: health.SignalFunc(l.verdictRefreshed), Reason: "the app stopped updating its health state"})
}

// start marks the app able to serve, so from now on the verdict follows the
// loops. Calling it again changes nothing.
func (l *liveness) start() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.serving.Store(true)
	l.updateLocked()
}

// drain turns the verdict unhealthy for good, at shutdown or a fatal exit.
func (l *liveness) drain() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.serving.Store(false)
	l.status.BeginDrain()
}

// run is the ticker: it sets the verdict at once and then every tick, until
// ctx ends.
func (l *liveness) run(ctx context.Context, every time.Duration) {
	tick := time.NewTicker(every)
	defer tick.Stop()
	for {
		l.mu.Lock()
		l.updateLocked()
		l.mu.Unlock()
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
		}
	}
}

// updateLocked sets the verdict healthy only while the app serves and every
// loop is inside its lease, and logs each loop that has just gone stale.
// Caller holds l.mu.
func (l *liveness) updateLocked() {
	healthy := l.serving.Load()
	for _, e := range l.loops() {
		ok := e.Healthy()
		if !ok && !l.stale[e] {
			slog.Warn("a loop stopped making progress", "loop", e.name, "lease", e.lease.String())
		}
		l.stale[e] = !ok
		healthy = healthy && ok
	}
	l.status.Set(healthy)
}
