package main

import (
	"context"
	"io"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cplieger/go/scheduler"
	"github.com/cplieger/go/slogx/capture"
	"github.com/cplieger/plex-language-sync/internal/cache"
	"github.com/cplieger/plex-language-sync/internal/deepscan"
	"github.com/cplieger/plex-language-sync/internal/notify"
	"github.com/cplieger/plex-language-sync/internal/obs"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/testsupport/fakeapi"
	"github.com/cplieger/plex-language-sync/internal/testsupport/metrictest"
	"github.com/cplieger/plex-language-sync/internal/users"
)

func TestLoadMetricsListenAddr(t *testing.T) {
	tests := []struct{ name, raw, want string }{
		{name: "unset", raw: "", want: ""},
		{name: "plain", raw: "127.0.0.1:9471", want: "127.0.0.1:9471"},
		{name: "invisible edge bytes", raw: " \u200b127.0.0.1:9471\t\u00a0", want: "127.0.0.1:9471"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("METRICS_LISTEN_ADDR", tt.raw)
			if got := loadMetricsListenAddr(); got != tt.want {
				t.Errorf("loadMetricsListenAddr() with %q = %q, want %q", tt.raw, got, tt.want)
			}
		})
	}
}

// Not parallel at the top: one case sets the environment.
func TestBindMetrics(t *testing.T) {
	t.Run("unset binds nothing", func(t *testing.T) {
		t.Parallel()
		ln, err := bindMetrics(t.Context(), "")
		if ln != nil || err != nil {
			t.Errorf("bindMetrics(\"\") = %v, %v; want no listener and no error", ln, err)
		}
	})
	t.Run("a trimmed address binds", func(t *testing.T) {
		t.Setenv("METRICS_LISTEN_ADDR", " 127.0.0.1:0\t")
		ln, err := bindMetrics(t.Context(), loadMetricsListenAddr())
		if err != nil {
			t.Fatalf("bindMetrics: %v", err)
		}
		defer ln.Close()
		if host, _, _ := net.SplitHostPort(ln.Addr().String()); host != "127.0.0.1" {
			t.Errorf("bound %s, want 127.0.0.1", ln.Addr())
		}
	})
	taken, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { taken.Close() })
	for _, addr := range []string{"not an address", taken.Addr().String()} {
		t.Run(addr, func(t *testing.T) {
			t.Parallel()
			ln, err := bindMetrics(t.Context(), addr)
			if err == nil {
				ln.Close()
				t.Fatalf("bindMetrics(%q) bound %s, want an error", addr, ln.Addr())
			}
			if !strings.Contains(err.Error(), "METRICS_LISTEN_ADDR") {
				t.Errorf("error %q does not name METRICS_LISTEN_ADDR", err)
			}
		})
	}
}

// Not parallel: run installs its own default logger, restored at cleanup.
func TestRunExitsOnAnUnbindableMetricsAddr(t *testing.T) {
	capture.Default(t)
	t.Setenv("PLEX_URL", "http://plex.invalid:32400")
	t.Setenv("PLEX_TOKEN", "test-token")
	t.Setenv("METRICS_LISTEN_ADDR", "not an address")
	if got := run(); got != 1 {
		t.Errorf("run() = %d with an unbindable METRICS_LISTEN_ADDR, want 1", got)
	}
}

// stampAt returns a Stamp holding one complete run dated at. Record always
// stamps the current time, so backdating writes the file format directly.
func stampAt(t *testing.T, at time.Time) *scheduler.Stamp {
	t.Helper()
	path := filepath.Join(t.TempDir(), "last-run")
	if err := os.WriteFile(path, []byte(at.UTC().Format(time.RFC3339Nano)+" ok\n"), 0o600); err != nil {
		t.Fatalf("seed stamp: %v", err)
	}
	return scheduler.NewStamp(path)
}

func TestSeedDeepScanFromTheStamp(t *testing.T) {
	t.Parallel()
	const lastSuccess = "plex_language_sync_deep_scan_last_success_timestamp_seconds"
	const runs = "plex_language_sync_deep_scan_runs_total"
	enabled := &config{schedulerEnabled: true, schedulerInterval: 24 * time.Hour}
	for _, age := range []time.Duration{time.Hour, 72 * time.Hour} {
		t.Run(age.String()+" old", func(t *testing.T) {
			t.Parallel()
			at := time.Now().Add(-age).Truncate(time.Second)
			m := obs.New()
			seedDeepScan(m, stampAt(t, at), enabled)
			if got := metrictest.Value(t, m, lastSuccess); got != float64(at.Unix()) {
				t.Errorf("the last complete scan = %v, want the record's %d", got, at.Unix())
			}
			for _, outcome := range []string{"complete", "incomplete"} {
				if got := metrictest.Value(t, m, runs, outcome); got != 0 {
					t.Errorf("seeding counted %v %s runs, want 0", got, outcome)
				}
			}
		})
	}
	t.Run("no record", func(t *testing.T) {
		t.Parallel()
		m := obs.New()
		seedDeepScan(m, scheduler.NewStamp(filepath.Join(t.TempDir(), "last-run")), enabled)
		if got := metrictest.Value(t, m, lastSuccess); got != 0 {
			t.Errorf("the last complete scan = %v with no record, want 0", got)
		}
	})
	t.Run("scan off", func(t *testing.T) {
		t.Parallel()
		m := obs.New()
		seedDeepScan(m, stampAt(t, time.Now().Add(-time.Hour)), &config{})
		if got := metrictest.Value(t, m, lastSuccess); got != 0 {
			t.Errorf("the last complete scan = %v with the scan off, want 0", got)
		}
	})
}

func TestServeMetrics(t *testing.T) {
	t.Parallel()
	ln, err := bindMetrics(t.Context(), "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(t.Context())
	done := serveMetrics(ctx, ln, obs.New())
	t.Cleanup(func() { cancel(); <-done })
	url := "http://" + ln.Addr().String() + "/metrics"

	req, _ := http.NewRequestWithContext(t.Context(), http.MethodGet, url, http.NoBody)
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	body, _ := io.ReadAll(resp.Body)
	resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Errorf("GET /metrics = %d, want 200", resp.StatusCode)
	}
	for _, family := range []string{"plex_language_sync_websocket_connected", "process_start_time_seconds"} {
		if !strings.Contains(string(body), "# TYPE "+family+" ") {
			t.Errorf("/metrics carries no %s family", family)
		}
	}

	req, _ = http.NewRequestWithContext(t.Context(), http.MethodPost, url, http.NoBody)
	resp, err = http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	resp.Body.Close()
	if resp.StatusCode != http.StatusMethodNotAllowed {
		t.Errorf("POST /metrics = %d, want 405", resp.StatusCode)
	}
}

func TestNotifyAdapterRecordsConnectionState(t *testing.T) {
	t.Parallel()
	m := obs.New()
	adapter := &notifyAdapter{metrics: m}
	adapter.OnConnection(t.Context(), true)
	if got := metrictest.Value(t, m, "plex_language_sync_websocket_connected"); got != 1 {
		t.Errorf("connected = %v after the handshake, want 1", got)
	}
	adapter.OnConnection(t.Context(), false)
	if got := metrictest.Value(t, m, "plex_language_sync_websocket_connected"); got != 0 {
		t.Errorf("connected = %v after a drop, want 0", got)
	}
	if got := metrictest.Value(t, m, "plex_language_sync_websocket_reconnects_total"); got != 1 {
		t.Errorf("reconnects = %v after one drop, want 1", got)
	}
}

func TestPlayEventsCountedByOutcome(t *testing.T) {
	t.Parallel()
	m := obs.New()
	_, client := newPlayServer(t)
	adapter, _ := newPlayAdapter(t, client)
	adapter.metrics = m

	adapter.OnPlay(t.Context(), playOfFirst)
	adapter.OnPlay(t.Context(), notify.PlayEvent{State: "playing", RatingKey: "2001"})
	adapter.OnPlay(t.Context(), notify.PlayEvent{State: "stopped", RatingKey: "2001", ClientIdentifier: "mac-A"})

	if got := metrictest.Value(t, m, "plex_language_sync_play_events_total", "attributed"); got != 1 {
		t.Errorf("attributed = %v, want 1", got)
	}
	if got := metrictest.Value(t, m, "plex_language_sync_play_events_total", "skipped"); got != 1 {
		t.Errorf("skipped = %v, want 1 (an irrelevant event is not a play event)", got)
	}
}

// Not parallel: skipUnattributedPlayEvent logs through the default logger.
func TestResolutionStallGaugeHoldsUntilRecovery(t *testing.T) {
	capture.Default(t)
	m := obs.New()
	_, client := newPlayServer(t)
	adapter, _ := newPlayAdapter(t, client)
	adapter.metrics, adapter.resolveStalls = m, &resolveStallCounter{}
	const family = "plex_language_sync_user_resolution_stalled"

	ev := notify.PlayEvent{State: "playing", RatingKey: "2001", ClientIdentifier: "mac-A"}
	for range resolveStallThreshold {
		adapter.skipUnattributedPlayEvent(ev, errSessionsUnreadable)
	}
	if got := metrictest.Value(t, m, family, obs.StallSessionsUnreadable); got != 1 {
		t.Errorf("stalled{sessions_unreadable} = %v after the stall, want 1", got)
	}
	adapter.OnPlay(t.Context(), playOfFirst)
	if got := metrictest.Value(t, m, family, obs.StallSessionsUnreadable); got != 0 {
		t.Errorf("stalled{sessions_unreadable} = %v after an attributed play, want 0", got)
	}
}

// Both save paths go through persister.save: the deep scan's flush at the end
// of a pass, and the save at shutdown. Not parallel: the pass logs through the
// default logger.
func TestCacheSaveFailuresCountedOnBothPaths(t *testing.T) {
	capture.Default(t)
	m := obs.New()
	notADir := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(notADir, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	c := cache.New()
	p := &persister{cache: c, dir: notADir, metrics: m}
	const family = "plex_language_sync_cache_save_failures_total"

	mgr := users.NewManager(c, nil)
	mgr.Init(&plex.User{ID: "1", Name: "admin"})
	sched := deepscan.New(deepscan.Config{Enable: true, Interval: time.Hour}, &deepscan.Deps{
		Plex: &fakeapi.Plex{}, Cache: c, Stamp: scheduler.NewStamp(filepath.Join(t.TempDir(), "stamp")),
		UserClient: func(string) deepscan.EpisodeReader { return nil },
		Sync:       newTimelineSyncer(c, mgr, false), SaveCache: p.save,
	})
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	sched.Run(ctx)
	if got := metrictest.Value(t, m, family); got != 1 {
		t.Errorf("cache save failures = %v after a failed deep-scan save, want 1", got)
	}

	saveOnShutdown(func() {}, p.save)
	if got := metrictest.Value(t, m, family); got != 2 {
		t.Errorf("cache save failures = %v after a failed shutdown save, want 2", got)
	}
}
