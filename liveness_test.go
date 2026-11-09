package main

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/go/health"
	"github.com/cplieger/go/slogx/capture"
	"github.com/cplieger/plex-language-sync/internal/notify"
	"github.com/cplieger/plex-language-sync/internal/plex"
)

// defaultLeases are the leases of an install with every setting at its default.
func defaultLeases() loopLeases {
	return leasesFor(&config{schedulerEnabled: true, schedulerInterval: 24 * time.Hour}, notify.DefaultConfig())
}

// probe is the exit code the image's health subcommand gives the marker at path.
func probe(path string) int {
	if health.Inspect(path, probeOptions()...).Healthy() {
		return 0
	}
	return 1
}

// beatEvery beats each entry every minute until ctx ends, standing in for
// loops that keep working.
func beatEvery(ctx context.Context, entries ...*loopEntry) {
	tick := time.NewTicker(time.Minute)
	defer tick.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-tick.C:
			for _, e := range entries {
				e.Beat()
			}
		}
	}
}

func (e *loopEntry) isParked() bool {
	e.mu.Lock()
	defer e.mu.Unlock()
	return e.parked
}

func TestLeasesFor(t *testing.T) {
	t.Parallel()
	ws := notify.DefaultConfig()
	tests := []struct {
		name     string
		cfg      config
		deepScan time.Duration
	}{
		// Two intervals plus the two hours one pass may take.
		{"default interval", config{schedulerEnabled: true, schedulerInterval: 24 * time.Hour}, 50 * time.Hour},
		{"six hours", config{schedulerEnabled: true, schedulerInterval: 6 * time.Hour}, 14 * time.Hour},
		{"scan off", config{schedulerEnabled: false}, 0},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := leasesFor(&tt.cfg, ws)
			if d := got.deepScan; d != tt.deepScan {
				t.Errorf("deep scan lease = %v, want %v", d, tt.deepScan)
			}
			// Two one-hour read deadlines: one for the read, one for the
			// handler of the message read last.
			if d := got.websocket; d != 2*time.Hour {
				t.Errorf("websocket lease = %v, want 2h", d)
			}
			// One 30-second backoff plus one five-minute connect attempt.
			if d := got.reconnect; d != 5*time.Minute+30*time.Second {
				t.Errorf("reconnect lease = %v, want 5m30s", d)
			}
		})
	}
}

// Each loop's receiver from wire arms that loop's entry and no other, so no
// loop runs on another loop's lease.
func TestLivenessWireGivesEachLoopItsOwnEntry(t *testing.T) {
	t.Parallel()
	tests := []struct {
		beat func(ws notify.Config, deepScan *loopEntry)
		name string
	}{
		{func(ws notify.Config, _ *loopEntry) { ws.ReadLoop.Beat() }, "websocket"},
		{func(ws notify.Config, _ *loopEntry) { ws.ReconnectLoop.Beat() }, "reconnect"},
		{func(_ notify.Config, deepScan *loopEntry) { deepScan.Beat() }, "deep_scan"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			live := newLiveness(filepath.Join(t.TempDir(), "healthy"), defaultLeases())
			ws, deepScan := live.wire(notify.DefaultConfig())
			tt.beat(ws, deepScan)
			for _, e := range live.loops() {
				if armed := !e.isParked(); armed != (e.name == tt.name) {
					t.Errorf("after the %s receiver beat, %s entry armed = %v, want %v", tt.name, e.name, armed, e.name == tt.name)
				}
			}
		})
	}
}

// Each loop wedged in turn while the others keep beating: the probe passes
// until that loop's lease runs out, fails within the ticker's lease after it,
// and the log names the loop.
func TestLivenessWedgedLoopTurnsTheProbeUnhealthy(t *testing.T) {
	for _, name := range []string{"websocket", "reconnect", "deep_scan"} {
		t.Run(name, func(t *testing.T) {
			rec := capture.Default(t)
			synctest.Test(t, func(t *testing.T) {
				path := filepath.Join(t.TempDir(), "healthy")
				live := newLiveness(path, defaultLeases())
				var wedged *loopEntry
				var others []*loopEntry
				for _, e := range live.loops() {
					e.Beat()
					if e.name == name {
						wedged = e
					} else {
						others = append(others, e)
					}
				}
				if wedged == nil {
					t.Fatalf("no loop named %q", name)
				}
				ctx, cancel := context.WithCancel(t.Context())
				defer cancel()
				go live.run(ctx, livenessTick)
				go beatEvery(ctx, others...)
				live.start()

				time.Sleep(wedged.lease - livenessTick)
				synctest.Wait()
				if got := probe(path); got != 0 {
					t.Fatalf("probe before the %s lease ran out = %d, want 0", name, got)
				}
				time.Sleep(livenessTick + markerLease.Duration())
				synctest.Wait()
				if got := probe(path); got != 1 {
					t.Errorf("probe one ticker lease after the %s lease ran out = %d, want 1", name, got)
				}
			})
			if !rec.HasAttr("a loop stopped making progress", "loop", name) {
				t.Errorf("no warning naming loop %q; got %v", name, rec.Messages())
			}
		})
	}
}

// With DEEP_SCAN_INTERVAL=off the deep-scan entry is not checked, so a deep
// scan that never beats again stays healthy.
func TestLivenessScanOffSilentDeepScanStaysHealthy(t *testing.T) {
	t.Setenv("DEEP_SCAN_INTERVAL", "off")
	var cfg config
	cfg.schedulerInterval, cfg.schedulerEnabled = loadSchedulerInterval()
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "healthy")
		live := newLiveness(path, leasesFor(&cfg, notify.DefaultConfig()))
		for _, e := range live.loops() {
			e.Beat()
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go live.run(ctx, livenessTick)
		go beatEvery(ctx, live.websocket, live.reconnect)
		live.start()

		time.Sleep(100 * time.Hour)
		synctest.Wait()
		if got := probe(path); got != 0 {
			t.Errorf("probe after 100h of a silent deep scan with the scan off = %d, want 0", got)
		}
	})
}

// A parked loop is idle by design: while Plex is unreachable only the
// reconnect loop runs, and the read loop and a deep scan that has not started
// stay healthy for as long as the outage lasts.
func TestLivenessParkedLoopIsNoFault(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "healthy")
		live := newLiveness(path, defaultLeases())
		live.reconnect.Beat()
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go live.run(ctx, livenessTick)
		go beatEvery(ctx, live.reconnect)
		live.start()

		time.Sleep(100 * time.Hour)
		synctest.Wait()
		if got := probe(path); got != 0 {
			t.Errorf("probe after a 100h outage with only the reconnect loop beating = %d, want 0", got)
		}
	})
}

// The marker turns healthy only once the app can serve, and turns unhealthy
// for good at the drain, whatever the loops report.
func TestLivenessStartAndDrain(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		path := filepath.Join(t.TempDir(), "healthy")
		live := newLiveness(path, defaultLeases())
		for _, e := range live.loops() {
			e.Beat()
		}
		ctx, cancel := context.WithCancel(t.Context())
		defer cancel()
		go live.run(ctx, livenessTick)

		time.Sleep(time.Minute)
		synctest.Wait()
		if got := probe(path); got != 1 {
			t.Errorf("probe before start = %d, want 1", got)
		}
		live.start()
		if got := probe(path); got != 0 {
			t.Errorf("probe right after start = %d, want 0", got)
		}
		live.drain()
		if got := probe(path); got != 1 {
			t.Errorf("probe right after the drain = %d, want 1", got)
		}
		for _, e := range live.loops() {
			e.Beat()
		}
		time.Sleep(time.Minute)
		synctest.Wait()
		if got := probe(path); got != 1 {
			t.Errorf("probe a minute after the drain = %d, want 1 (a tick must not restore it)", got)
		}
	})
}

// The ticker refreshes the marker on every tick, and once it stops the marker
// ages past the probe's deadline. Real time: the marker's age is its file
// time, which a synctest clock does not move.
func TestLivenessStoppedTickerTurnsTheProbeUnhealthy(t *testing.T) {
	path := filepath.Join(t.TempDir(), "healthy")
	live := newLiveness(path, defaultLeases())
	for _, e := range live.loops() {
		e.Beat()
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := make(chan struct{})
	go func() {
		defer close(done)
		live.run(ctx, 10*time.Millisecond)
	}()
	live.start()

	old := time.Now().Add(-time.Hour)
	if err := os.Chtimes(path, old, old); err != nil {
		t.Fatal(err)
	}
	for deadline := time.Now().Add(5 * time.Second); probe(path) != 0; time.Sleep(5 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("a running ticker did not refresh an hour-old marker within 5s")
		}
	}

	cancel()
	<-done
	aged := time.Now().Add(-markerLease.Duration() - time.Second)
	if err := os.Chtimes(path, aged, aged); err != nil {
		t.Fatal(err)
	}
	if got := probe(path); got != 1 {
		t.Errorf("probe on a marker older than its lease after the ticker stopped = %d, want 1", got)
	}
}

// An unreachable Plex at boot is the reconnect loop's work, not a fault: the
// connect retry starts the app degraded and beats the reconnect entry, and
// parks it once it returns.
func TestConnectAndResolveAdminUnreachablePlexBeatsTheReconnectLoop(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusServiceUnavailable)
	}))
	defer srv.Close()
	client, err := plex.NewClient(plex.Options{ServerURL: srv.URL, Token: "test-token"})
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(t.TempDir(), "healthy")
	live := newLiveness(path, defaultLeases())
	ctx, cancel := context.WithCancel(t.Context())
	defer cancel()
	errc := make(chan error, 1)
	go func() {
		_, _, err := connectAndResolveAdmin(ctx, client, live)
		errc <- err
	}()

	for deadline := time.Now().Add(10 * time.Second); probe(path) != 0; time.Sleep(10 * time.Millisecond) {
		if time.Now().After(deadline) {
			t.Fatal("an unreachable Plex did not start the app degraded within 10s")
		}
	}
	if live.reconnect.isParked() {
		t.Error("the connect retry did not beat the reconnect entry")
	}
	cancel()
	if err := <-errc; !errors.Is(err, context.Canceled) {
		t.Errorf("connectAndResolveAdmin after cancel = %v, want context.Canceled", err)
	}
	if !live.reconnect.isParked() {
		t.Error("the reconnect entry stayed armed after the connect retry returned")
	}
}
