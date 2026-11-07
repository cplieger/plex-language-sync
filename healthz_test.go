package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/go/slogx/capture"
	"github.com/cplieger/plex-language-sync/internal/obs"
)

// The reasons /healthz lists, one fixed sentence per check.
const (
	reasonServing   = "the app is starting or shutting down"
	reasonWebSocket = "the Plex WebSocket listener stopped making progress"
	reasonReconnect = "the Plex reconnect loop stopped making progress"
	reasonDeepScan  = "the deep scan stopped making progress"
	reasonTicker    = "the app stopped updating its health state"
)

// healthz is one answer of the listener's GET /healthz.
type healthz struct {
	Status  string   `json:"status"`
	Reasons []string `json:"reasons"`
	code    int
}

func getHealthz(t *testing.T, h http.Handler, method string) healthz {
	t.Helper()
	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), method, "/healthz", http.NoBody))
	got := healthz{code: rec.Code}
	if method == http.MethodGet {
		if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
			t.Fatalf("GET /healthz body %q: %v", rec.Body.String(), err)
		}
	}
	return got
}

func TestHealthzRoute(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		live := newLiveness(filepath.Join(t.TempDir(), "healthy"), defaultLeases())
		h := newListenerMux(obs.New(), live)

		got := getHealthz(t, h, http.MethodGet)
		if got.code != http.StatusServiceUnavailable || got.Status != "unhealthy" || !slices.Equal(got.Reasons, []string{reasonServing}) {
			t.Errorf("GET /healthz before start = %d %+v, want 503 unhealthy [%q]", got.code, got, reasonServing)
		}

		live.start()
		got = getHealthz(t, h, http.MethodGet)
		if got.code != http.StatusOK || got.Status != "ok" || got.Reasons != nil {
			t.Errorf("GET /healthz once serving = %d %+v, want 200 ok with no reasons", got.code, got)
		}
		rec := httptest.NewRecorder()
		h.ServeHTTP(rec, httptest.NewRequestWithContext(t.Context(), http.MethodGet, "/healthz", http.NoBody))
		for header, want := range map[string]string{
			"Content-Type": "application/json", "Cache-Control": "no-store", "X-Content-Type-Options": "nosniff",
		} {
			if v := rec.Header().Get(header); v != want {
				t.Errorf("GET /healthz %s = %q, want %q", header, v, want)
			}
		}
		if got := getHealthz(t, h, http.MethodHead); got.code != http.StatusOK {
			t.Errorf("HEAD /healthz = %d, want 200", got.code)
		}
		if got := getHealthz(t, h, http.MethodPost); got.code != http.StatusMethodNotAllowed {
			t.Errorf("POST /healthz = %d, want 405", got.code)
		}
	})
}

// verdicts reads /healthz and the health subcommand at one instant.
func verdicts(t *testing.T, h http.Handler, path string) (healthz, int) {
	t.Helper()
	return getHealthz(t, h, http.MethodGet), probe(path)
}

// agree fails unless /healthz and the probe give the same verdict, and
// /healthz lists exactly reasons.
func agree(t *testing.T, when string, h http.Handler, path string, reasons ...string) {
	t.Helper()
	got, code := verdicts(t, h, path)
	wantCode, wantProbe := http.StatusOK, 0
	if len(reasons) > 0 {
		wantCode, wantProbe = http.StatusServiceUnavailable, 1
	}
	if got.code != wantCode || !slices.Equal(got.Reasons, reasons) {
		t.Errorf("%s: GET /healthz = %d %q, want %d %q", when, got.code, got.Reasons, wantCode, reasons)
	}
	if code != wantProbe {
		t.Errorf("%s: probe = %d, want %d", when, code, wantProbe)
	}
}

// newAgreement wires a liveness, its ticker and the listener's mux, every
// loop beaten once; stop ends the ticker.
func newAgreement(t *testing.T) (live *liveness, h http.Handler, path string, stop context.CancelFunc) {
	t.Helper()
	path = filepath.Join(t.TempDir(), "healthy")
	live = newLiveness(path, defaultLeases())
	for _, e := range live.loops() {
		e.Beat()
	}
	ctx, stop := context.WithCancel(t.Context())
	t.Cleanup(stop)
	go live.run(ctx, livenessTick)
	return live, newListenerMux(obs.New(), live), path, stop
}

func TestHealthzAgreesWithTheProbe(t *testing.T) {
	t.Run("starting, serving and draining", func(t *testing.T) {
		capture.Default(t)
		synctest.Test(t, func(t *testing.T) {
			live, h, path, _ := newAgreement(t)
			synctest.Wait()
			agree(t, "before start", h, path, reasonServing)
			live.start()
			agree(t, "serving", h, path)
			live.drain()
			agree(t, "draining", h, path, reasonServing)
		})
	})

	for name, reason := range map[string]string{
		"websocket": reasonWebSocket, "reconnect": reasonReconnect, "deep_scan": reasonDeepScan,
	} {
		t.Run("wedged "+name, func(t *testing.T) {
			capture.Default(t)
			synctest.Test(t, func(t *testing.T) {
				live, h, path, _ := newAgreement(t)
				var wedged *loopEntry
				var others []*loopEntry
				for _, e := range live.loops() {
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
				go beatEvery(ctx, others...)
				live.start()

				time.Sleep(wedged.lease - livenessTick)
				synctest.Wait()
				agree(t, "before the "+name+" lease ran out", h, path)
				time.Sleep(livenessTick + markerLease.Duration())
				synctest.Wait()
				agree(t, "one ticker lease after the "+name+" lease ran out", h, path, reason)
			})
		})
	}

	t.Run("ticker stopped", func(t *testing.T) {
		capture.Default(t)
		synctest.Test(t, func(t *testing.T) {
			live, h, path, stop := newAgreement(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go beatEvery(ctx, live.loops()...)
			live.start()
			synctest.Wait()
			agree(t, "ticker running", h, path)

			stop()
			synctest.Wait()
			// The marker's age is its file time, which the bubble's clock does
			// not move: date the last write at the bubble's now.
			now := time.Now()
			if err := os.Chtimes(path, now, now); err != nil {
				t.Fatal(err)
			}
			time.Sleep(markerLease.Duration() + time.Second)
			synctest.Wait()
			agree(t, "one marker lease after the ticker stopped", h, path, reasonTicker)
		})
	})

	// A marker path os.Stat cannot read fails every refresh and the probe,
	// with the ticker still running.
	t.Run("marker unreadable", func(t *testing.T) {
		capture.Default(t)
		synctest.Test(t, func(t *testing.T) {
			live, h, path, _ := newAgreement(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go beatEvery(ctx, live.loops()...)
			live.start()
			synctest.Wait()
			agree(t, "marker written", h, path)

			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Base(path), path); err != nil {
				t.Fatal(err)
			}
			time.Sleep(livenessTick)
			synctest.Wait()
			agree(t, "one tick after the marker became a symlink loop", h, path, reasonTicker)
		})
	})

	// A dangling symlink at the marker path reads as absent, and every
	// refresh fails to create the file through it.
	t.Run("marker absent while the app is healthy", func(t *testing.T) {
		capture.Default(t)
		synctest.Test(t, func(t *testing.T) {
			live, h, path, _ := newAgreement(t)
			ctx, cancel := context.WithCancel(t.Context())
			defer cancel()
			go beatEvery(ctx, live.loops()...)
			live.start()
			synctest.Wait()
			agree(t, "marker written", h, path)

			if err := os.Remove(path); err != nil {
				t.Fatal(err)
			}
			if err := os.Symlink(filepath.Join(filepath.Dir(path), "missing", "x"), path); err != nil {
				t.Fatal(err)
			}
			time.Sleep(livenessTick)
			synctest.Wait()
			agree(t, "one tick after the marker became a dangling symlink", h, path, reasonTicker)
		})
	})

	t.Run("ticker stopped while the marker is absent", func(t *testing.T) {
		capture.Default(t)
		synctest.Test(t, func(t *testing.T) {
			live, h, path, stop := newAgreement(t)
			live.start()
			time.Sleep(live.reconnect.lease + 2*livenessTick)
			synctest.Wait()
			agree(t, "reconnect loop stale", h, path, reasonReconnect)

			stop()
			synctest.Wait()
			for _, e := range live.loops() {
				e.Beat()
			}
			time.Sleep(markerLease.Duration() + time.Second)
			synctest.Wait()
			agree(t, "loops recovered with the ticker stopped", h, path, reasonTicker)
		})
	})
}
