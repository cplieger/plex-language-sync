package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	"github.com/cplieger/go/health"
	"github.com/cplieger/plex-language-sync/internal/notify"
)

// livenessTick is how often the ticker reads the loops and rewrites the marker.
const livenessTick = 10 * time.Second

// markerLease is the probe's deadline on the marker: three ticks, so a late
// tick passes and only a stopped ticker fails it.
var markerLease = health.Lease{Interval: livenessTick, Cycles: 3}

// deepScanPassAllowance is the longest one deep-scan pass may run, the two
// hours the deep scan alerts allow on top of two intervals.
const deepScanPassAllowance = 2 * time.Hour

// connectAttemptBudget is the longest one connect attempt may take: the
// startup attempt makes two Plex requests, each bounded by plexapi's
// two-minute request timeout.
const connectAttemptBudget = 5 * time.Minute

// probeOptions are the health subcommand's: the marker must be no older than
// markerLease.
func probeOptions() []health.ProbeOption {
	return []health.ProbeOption{health.WithMaxAge(markerLease.Duration())}
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
	last   time.Time
	name   string
	lease  time.Duration
	mu     sync.Mutex
	parked bool
}

func newLoopEntry(name string, lease time.Duration) *loopEntry {
	return &loopEntry{name: name, lease: lease, parked: true}
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

// liveness is the one owner of the health marker. Each loop beats its own
// entry and one ticker writes the marker from all of them, so a wedged loop
// turns the container unhealthy while the others keep working.
type liveness struct {
	latch     *health.Latch
	websocket *loopEntry
	reconnect *loopEntry
	deepScan  *loopEntry
	// stale holds the loops already reported stale, so each stall logs once.
	stale   map[*loopEntry]bool
	mu      sync.Mutex
	serving bool
}

// newLiveness returns a liveness writing marker, every entry parked and the
// app not yet serving.
func newLiveness(marker *health.Marker, leases loopLeases) *liveness {
	return &liveness{
		latch:     health.NewLatch(marker),
		websocket: newLoopEntry("websocket", leases.websocket),
		reconnect: newLoopEntry("reconnect", leases.reconnect),
		deepScan:  newLoopEntry("deep_scan", leases.deepScan),
		stale:     map[*loopEntry]bool{},
	}
}

func (l *liveness) loops() []*loopEntry {
	return []*loopEntry{l.websocket, l.reconnect, l.deepScan}
}

// start marks the app able to serve, so from now on the marker follows the
// loops. Calling it again changes nothing.
func (l *liveness) start() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.serving = true
	l.updateLocked()
}

// drain turns the marker unhealthy for good, at shutdown or a fatal exit.
func (l *liveness) drain() {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.serving = false
	l.latch.BeginDrain()
}

// run is the ticker: it rewrites the marker at once and then every tick,
// until ctx ends.
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

// updateLocked writes the marker healthy only while the app serves and every
// loop is inside its lease, and logs each loop that has just gone stale.
// Caller holds l.mu.
func (l *liveness) updateLocked() {
	healthy := l.serving
	for _, e := range l.loops() {
		ok := e.Healthy()
		if !ok && !l.stale[e] {
			slog.Warn("a loop stopped making progress", "loop", e.name, "lease", e.lease.String())
		}
		l.stale[e] = !ok
		healthy = healthy && ok
	}
	l.latch.Set(healthy)
}
