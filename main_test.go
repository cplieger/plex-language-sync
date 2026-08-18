// main_test.go holds tests for the composition-root concerns that
// remain in the main package after the cycle-1 extraction:
//
//   - Configuration loading (loadConfig + env helpers).
//   - Validation helpers (splitTrim, requireEnv with _FILE secret
//     handling via envx.Secret) and the scheduler
//     interval parser (loadSchedulerInterval).
//   - notifyAdapter trigger-gate behaviour (the dispatch guards that
//     live alongside the WS listener).
//
// Composition-root wiring is verified indirectly: run() is the wiring
// layer, and every collaborator it assembles (users.Manager, cache,
// scheduler, syncer, notify listener) has its own test suite under
// internal/*. There is no dedicated TestRun because every branch of
// run() is either startup plumbing (plex connectivity, env loading
// already covered here) or a fan-out into an already-tested
// subsystem.
//
// Business-logic tests that used to live here moved out:
//
//   - Track-sync / language-profile / stream-apply / episode-ref
//     tests → internal/tracksync/tracks_test.go.
//   - Scheduler worker-pool / dedup / circuit-breaker tests →
//     internal/deepscan/deepscan_test.go.
//   - User-manager tests → internal/users/manager_test.go (since
//     cycle-1 step 6).
//   - WebSocket listener tests → internal/notify/*_test.go (since
//     cycle-1 step 5).
//   - Plex HTTP client tests → internal/plex/client_test.go (since
//     cycle-1 step 3).
//   - Stream-selection helpers → internal/streams/*_test.go (since
//     cycle-1 step 2).
//   - Cache persistence tests → internal/cache/cache_test.go (since
//     cycle-1 step 4).

package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/plex-language-sync/internal/api"
	"github.com/cplieger/plex-language-sync/internal/cache"
	"github.com/cplieger/plex-language-sync/internal/notify"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plex-language-sync/internal/testsupport/plexclient"
	"github.com/cplieger/plex-language-sync/internal/tracksync"
	"github.com/cplieger/plex-language-sync/internal/users"
)

// ---------------------------------------------------------------------------
// envBool / envOr / splitTrim
// ---------------------------------------------------------------------------

func TestSplitTrim(t *testing.T) {
	got := splitTrim(" foo , bar , , baz ")
	if len(got) != 3 || got[0] != "foo" || got[1] != "bar" || got[2] != "baz" {
		t.Errorf("splitTrim = %v, want [foo bar baz]", got)
	}
}

func TestSplitTrimEdgeCases(t *testing.T) {
	tests := []struct {
		name  string
		input string
		want  []string
	}{
		{"empty", "", []string{}},
		{"whitespace only", "   ", []string{}},
		{"single element", "foo", []string{"foo"}},
		{"trailing comma", "foo,", []string{"foo"}},
		{"leading comma", ",foo", []string{"foo"}},
		{"multiple empty", ",,,", []string{}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := splitTrim(tt.input)
			if len(got) != len(tt.want) {
				t.Errorf("splitTrim(%q) = %v, want %v", tt.input, got, tt.want)
				return
			}
			for i := range got {
				if got[i] != tt.want[i] {
					t.Errorf("splitTrim(%q)[%d] = %q, want %q", tt.input, i, got[i], tt.want[i])
				}
			}
		})
	}
}

// ---------------------------------------------------------------------------
// loadSchedulerInterval
// ---------------------------------------------------------------------------

func TestLoadSchedulerInterval(t *testing.T) {
	tests := []struct {
		name         string
		val          string
		wantInterval time.Duration
		wantEnabled  bool
	}{
		{"unset defaults to 24h", "", 24 * time.Hour, true},
		{"valid duration", "12h", 12 * time.Hour, true},
		{"minutes", "90m", 90 * time.Minute, true},
		{"off disables", "off", 0, false},
		{"disabled disables", "disabled", 0, false},
		{"OFF case-insensitive", "OFF", 0, false},
		{"zero disables", "0", 0, false},
		{"zero seconds disables", "0s", 0, false},
		{"bogus falls back to default", "notaduration", 24 * time.Hour, true},
		{"negative falls back to default", "-5h", 24 * time.Hour, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("SCHEDULER_INTERVAL", tt.val)
			gotInterval, gotEnabled := loadSchedulerInterval()
			if gotInterval != tt.wantInterval {
				t.Errorf("loadSchedulerInterval(%q) interval = %v, want %v", tt.val, gotInterval, tt.wantInterval)
			}
			if gotEnabled != tt.wantEnabled {
				t.Errorf("loadSchedulerInterval(%q) enabled = %v, want %v", tt.val, gotEnabled, tt.wantEnabled)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// loadConfig
// ---------------------------------------------------------------------------

func TestLoadConfig(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("PLEX_TOKEN", "test-token")
	t.Setenv("UPDATE_LEVEL", "season")
	t.Setenv("UPDATE_STRATEGY", "all")
	t.Setenv("TRIGGER_ON_PLAY", "true")
	t.Setenv("TRIGGER_ON_SCAN", "false")
	t.Setenv("LANGUAGE_PROFILES", "false")
	t.Setenv("SCHEDULER_INTERVAL", "12h")
	t.Setenv("IGNORE_LABELS", "SKIP,NOPE")
	t.Setenv("IGNORE_LIBRARIES", "Music,Photos")
	t.Setenv("DEBUG", "false")

	cfg := loadConfig()

	if cfg.plexURL != "http://plex:32400" {
		t.Errorf("plexURL = %q", cfg.plexURL)
	}
	if cfg.updateLevel != "season" {
		t.Errorf("updateLevel = %q, want season", cfg.updateLevel)
	}
	if cfg.updateStrategy != "all" {
		t.Errorf("updateStrategy = %q, want all", cfg.updateStrategy)
	}
	if !cfg.triggerOnPlay {
		t.Error("triggerOnPlay should be true")
	}
	if cfg.triggerOnScan {
		t.Error("triggerOnScan should be false")
	}
	if cfg.languageProfiles {
		t.Error("languageProfiles should be false")
	}
	if cfg.schedulerInterval != 12*time.Hour {
		t.Errorf("schedulerInterval = %v, want 12h", cfg.schedulerInterval)
	}
	if !cfg.schedulerEnabled {
		t.Error("schedulerEnabled should be true")
	}
	if len(cfg.ignoreLabels) != 2 || cfg.ignoreLabels[0] != "SKIP" {
		t.Errorf("ignoreLabels = %v", cfg.ignoreLabels)
	}
	if len(cfg.ignoreLibraries) != 2 || cfg.ignoreLibraries[0] != "Music" {
		t.Errorf("ignoreLibraries = %v", cfg.ignoreLibraries)
	}
}

func TestLoadConfigDefaults(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("PLEX_TOKEN", "test-token")
	t.Setenv("UPDATE_LEVEL", "")
	t.Setenv("UPDATE_STRATEGY", "")
	t.Setenv("TRIGGER_ON_PLAY", "")
	t.Setenv("TRIGGER_ON_SCAN", "")
	t.Setenv("LANGUAGE_PROFILES", "")
	t.Setenv("SCHEDULER_INTERVAL", "")
	t.Setenv("IGNORE_LABELS", "")
	t.Setenv("IGNORE_LIBRARIES", "")
	t.Setenv("DEBUG", "")
	t.Setenv("PLEX_URL_FILE", "")
	t.Setenv("PLEX_TOKEN_FILE", "")

	cfg := loadConfig()

	if cfg.updateLevel != "show" {
		t.Errorf("updateLevel = %q, want show", cfg.updateLevel)
	}
	if cfg.updateStrategy != "all" {
		t.Errorf("updateStrategy = %q, want all", cfg.updateStrategy)
	}
	if !cfg.triggerOnPlay {
		t.Error("triggerOnPlay should default to true")
	}
	if !cfg.triggerOnScan {
		t.Error("triggerOnScan should default to true")
	}
	if cfg.schedulerInterval != 24*time.Hour {
		t.Errorf("schedulerInterval should default to 24h, got %v", cfg.schedulerInterval)
	}
	if !cfg.schedulerEnabled {
		t.Error("schedulerEnabled should default to true")
	}
	if len(cfg.ignoreLabels) != 2 {
		t.Errorf("ignoreLabels should default to 2 items, got %v", cfg.ignoreLabels)
	}
}

func TestLoadConfigInvalidUpdateLevel(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("PLEX_TOKEN", "test-token")
	t.Setenv("UPDATE_LEVEL", "invalid")
	t.Setenv("UPDATE_STRATEGY", "invalid")
	t.Setenv("SCHEDULER_INTERVAL", "notaduration")
	t.Setenv("PLEX_URL_FILE", "")
	t.Setenv("PLEX_TOKEN_FILE", "")
	t.Setenv("IGNORE_LABELS", "")
	t.Setenv("IGNORE_LIBRARIES", "")
	t.Setenv("DEBUG", "")

	cfg := loadConfig()

	if cfg.updateLevel != "show" {
		t.Errorf("invalid updateLevel should default to show, got %q", cfg.updateLevel)
	}
	if cfg.updateStrategy != "all" {
		t.Errorf("invalid updateStrategy should default to all, got %q", cfg.updateStrategy)
	}
	if cfg.schedulerInterval != 24*time.Hour {
		t.Errorf("invalid SCHEDULER_INTERVAL should default to 24h, got %v", cfg.schedulerInterval)
	}
}

func TestLoadConfigInvalidUpdateStrategy(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("PLEX_TOKEN", "test-token")
	t.Setenv("UPDATE_STRATEGY", "random")
	t.Setenv("UPDATE_LEVEL", "")
	t.Setenv("SCHEDULER_INTERVAL", "")
	t.Setenv("PLEX_URL_FILE", "")
	t.Setenv("PLEX_TOKEN_FILE", "")
	t.Setenv("IGNORE_LABELS", "")
	t.Setenv("IGNORE_LIBRARIES", "")
	t.Setenv("DEBUG", "")

	cfg := loadConfig()
	if cfg.updateStrategy != "all" {
		t.Errorf("invalid updateStrategy should default to all, got %q", cfg.updateStrategy)
	}
}

func TestLoadConfigValidNextStrategy(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("PLEX_TOKEN", "test-token")
	t.Setenv("UPDATE_STRATEGY", "next")
	t.Setenv("UPDATE_LEVEL", "")
	t.Setenv("SCHEDULER_INTERVAL", "")
	t.Setenv("PLEX_URL_FILE", "")
	t.Setenv("PLEX_TOKEN_FILE", "")
	t.Setenv("IGNORE_LABELS", "")
	t.Setenv("IGNORE_LIBRARIES", "")
	t.Setenv("DEBUG", "")

	cfg := loadConfig()
	if cfg.updateStrategy != "next" {
		t.Errorf("expected updateStrategy=next, got %q", cfg.updateStrategy)
	}
}

func TestLoadConfigDebugMode(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("PLEX_TOKEN", "test-token")
	t.Setenv("UPDATE_LEVEL", "")
	t.Setenv("UPDATE_STRATEGY", "")
	t.Setenv("TRIGGER_ON_PLAY", "")
	t.Setenv("TRIGGER_ON_SCAN", "")
	t.Setenv("LANGUAGE_PROFILES", "")
	t.Setenv("SCHEDULER_INTERVAL", "")
	t.Setenv("IGNORE_LABELS", "")
	t.Setenv("IGNORE_LIBRARIES", "")
	t.Setenv("DEBUG", "true")
	t.Setenv("PLEX_URL_FILE", "")
	t.Setenv("PLEX_TOKEN_FILE", "")

	cfg := loadConfig()
	if !cfg.debug {
		t.Error("debug should be true")
	}
}

func TestLoadConfigWithFileSecrets(t *testing.T) {
	dir := t.TempDir()
	urlFile := filepath.Join(dir, "plex_url.txt")
	tokenFile := filepath.Join(dir, "plex_token.txt")
	if err := os.WriteFile(urlFile, []byte("http://plex:32400\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("secret-token\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PLEX_URL", "")
	t.Setenv("PLEX_TOKEN", "")
	t.Setenv("PLEX_URL_FILE", urlFile)
	t.Setenv("PLEX_TOKEN_FILE", tokenFile)
	t.Setenv("UPDATE_LEVEL", "")
	t.Setenv("UPDATE_STRATEGY", "")
	t.Setenv("TRIGGER_ON_PLAY", "")
	t.Setenv("TRIGGER_ON_SCAN", "")
	t.Setenv("LANGUAGE_PROFILES", "")
	t.Setenv("SCHEDULER_INTERVAL", "")
	t.Setenv("IGNORE_LABELS", "")
	t.Setenv("IGNORE_LIBRARIES", "")
	t.Setenv("DEBUG", "")

	cfg := loadConfig()

	if cfg.plexURL != "http://plex:32400" {
		t.Errorf("plexURL = %q, want http://plex:32400", cfg.plexURL)
	}
	if cfg.plexToken != "secret-token" {
		t.Errorf("plexToken = %q, want secret-token", cfg.plexToken)
	}
}

func TestLogConfig(t *testing.T) {
	// logConfig must mask the Plex token: the security contract (README
	// "token never logged") is that the real token value never reaches
	// the logs — only the literal "configured".
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	cfg := &config{
		plexURL:           "http://plex:32400",
		plexToken:         "super-secret-token-value",
		updateLevel:       "show",
		updateStrategy:    "all",
		schedulerInterval: 24 * time.Hour,
		ignoreLabels:      []string{"SKIP"},
	}
	logConfig(cfg)

	out := buf.String()
	if strings.Contains(out, "super-secret-token-value") {
		t.Errorf("logConfig leaked the Plex token into the logs: %q", out)
	}
	if !strings.Contains(out, "plex_token=configured") {
		t.Errorf("logConfig should log plex_token=configured, got: %q", out)
	}
}

// ---------------------------------------------------------------------------
// requireEnv via _FILE
// ---------------------------------------------------------------------------

func TestRequireEnvFromFile(t *testing.T) {
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "secret.txt")
	if err := os.WriteFile(secretFile, []byte("  my-secret-value  \n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("TEST_SECRET", "")
	t.Setenv("TEST_SECRET_FILE", secretFile)

	got := requireEnv("TEST_SECRET")
	if got != "my-secret-value" {
		t.Errorf("requireEnv via _FILE = %q, want %q", got, "my-secret-value")
	}
}

// TestRequireEnvTrimsFileWhitespace pins the trimming split of
// responsibility against envx >= v1.5.0: envx removes at most ONE trailing
// line ending from a KEY_FILE value and returns every other byte as
// written (a secret may legitimately contain whitespace), so requireEnv
// itself must trim for the two values it reads — a Plex URL and a Plex
// token, where padding means a malformed URL or a 401. Every shape below
// reaches envx with whitespace still attached; the app must see none of
// it. Interior whitespace is left alone: TrimSpace trims edges, and a
// value with a space in the middle is a broken secret the operator should
// see verbatim rather than have silently rewritten.
func TestRequireEnvTrimsFileWhitespace(t *testing.T) {
	tests := []struct {
		name    string
		content string
		want    string
	}{
		{"trailing newline only", "plain-value\n", "plain-value"},
		{"no trailing newline", "plain-value", "plain-value"},
		{"surrounding spaces", "  padded-value  \n", "padded-value"},
		{"surrounding tabs", "\tpadded-value\t\n", "padded-value"},
		{"crlf line ending", "crlf-value\r\n", "crlf-value"},
		{"second trailing newline", "double-nl-value\n\n", "double-nl-value"},
		{"leading newline", "\nleading-nl-value\n", "leading-nl-value"},
		{"trailing spaces after newline", "trailing-ws-value\n  ", "trailing-ws-value"},
		{"interior space preserved", "  interior value  \n", "interior value"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			secretFile := filepath.Join(t.TempDir(), "secret.txt")
			if err := os.WriteFile(secretFile, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}

			t.Setenv("TEST_SECRET", "")
			t.Setenv("TEST_SECRET_FILE", secretFile)

			got := requireEnv("TEST_SECRET")
			if got != tt.want {
				t.Errorf("requireEnv via _FILE with content %q = %q, want %q", tt.content, got, tt.want)
			}
		})
	}
}

// TestRequireEnvTrimsEnvWhitespace pins the same trim on the KEY channel,
// which envx has always returned verbatim. Both channels must resolve to
// the same secret for the same key, so a padded PLEX_TOKEN cannot become a
// different token than the identical value delivered through a file.
func TestRequireEnvTrimsEnvWhitespace(t *testing.T) {
	t.Setenv("TEST_SECRET_FILE", "")
	t.Setenv("TEST_SECRET", "  padded-value\t")

	got := requireEnv("TEST_SECRET")
	if got != "padded-value" {
		t.Errorf("requireEnv via env = %q, want %q", got, "padded-value")
	}
}

// TestLoadConfigTrimsFileSecrets pins the trim end-to-end through
// loadConfig: a PLEX_URL_FILE / PLEX_TOKEN_FILE mount whose content
// carries surrounding whitespace must reach the config as the bare URL and
// token, because cfg.plexURL is concatenated into request URLs and
// cfg.plexToken goes out as the auth header.
func TestLoadConfigTrimsFileSecrets(t *testing.T) {
	dir := t.TempDir()
	urlFile := filepath.Join(dir, "plex_url.txt")
	tokenFile := filepath.Join(dir, "plex_token.txt")
	if err := os.WriteFile(urlFile, []byte("  http://plex:32400  \n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(tokenFile, []byte("\tsecret-token \n\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	t.Setenv("PLEX_URL", "")
	t.Setenv("PLEX_TOKEN", "")
	t.Setenv("PLEX_URL_FILE", urlFile)
	t.Setenv("PLEX_TOKEN_FILE", tokenFile)
	t.Setenv("UPDATE_LEVEL", "")
	t.Setenv("UPDATE_STRATEGY", "")
	t.Setenv("TRIGGER_ON_PLAY", "")
	t.Setenv("TRIGGER_ON_SCAN", "")
	t.Setenv("LANGUAGE_PROFILES", "")
	t.Setenv("SCHEDULER_INTERVAL", "")
	t.Setenv("IGNORE_LABELS", "")
	t.Setenv("IGNORE_LIBRARIES", "")
	t.Setenv("DEBUG", "")

	cfg := loadConfig()

	if cfg.plexURL != "http://plex:32400" {
		t.Errorf("plexURL = %q, want http://plex:32400", cfg.plexURL)
	}
	if cfg.plexToken != "secret-token" {
		t.Errorf("plexToken = %q, want secret-token", cfg.plexToken)
	}
}

// ---------------------------------------------------------------------------
// readSecretFile bounds
// ---------------------------------------------------------------------------

// ---------------------------------------------------------------------------
// notifyAdapter trigger gates
//
// The adapter's OnPlay / OnTimeline paths short-circuit when the
// corresponding trigger flag is disabled, and pass through when
// enabled. Deeper dispatch behaviour is exercised by the integration
// tests in internal/notify; this suite pins the trigger-gate
// short-circuit so the composition root retains the same dispatch
// surface the pre-extraction handleNotification enforced.
// ---------------------------------------------------------------------------

func newTestAdapter(t *testing.T, triggerOnPlay, triggerOnScan bool) *notifyAdapter {
	t.Helper()
	parsed, _ := url.Parse("http://example.test")
	c := cache.New()
	client := plexclient.NewFromHTTP(parsed, "test-token", plexclient.Options{})
	mgr := users.NewManager(c)
	mgr.Init(&plex.User{ID: "1", Name: "admin"})
	return &notifyAdapter{
		syncer: nil, // unused on the gated-off paths
		cfg:    &config{triggerOnPlay: triggerOnPlay, triggerOnScan: triggerOnScan},
		users:  mgr,
		client: client,
		cache:  c,
	}
}

func TestResolvePlayEventUser_noClientIdentifier_failsClosed(t *testing.T) {
	adapter := newTestAdapter(t, true, false)
	ev := notify.PlayEvent{State: "playing", RatingKey: "100"}

	uid, uname, ok := adapter.resolvePlayEventUser(t.Context(), ev)

	if ok {
		t.Errorf("resolvePlayEventUser ok = true for an event with no client identifier, got (%q, %q); the fail-closed skip regressed (the event would be misattributed)", uid, uname)
	}
}

// NOTE: deep-dispatch tests for notifyAdapter.handlePlayEvent and
// handleTimeline (fetching episodes, dedup, ignored libraries, ignored
// shows, session resolution) live in internal/tracksync and internal/notify
// now — the per-feature logic moved out of the main package in
// cycle-1 steps 5 and 7. What remains here is the trigger-gate
// behaviour, which is a composition-root concern.

func TestWaitForBackgroundLoops_bothLoopsDone_returnsBeforeBudget(t *testing.T) {
	t.Parallel()
	var wg sync.WaitGroup
	wg.Add(2)
	refreshDone := make(chan struct{})
	schedDone := make(chan struct{})

	// Both background loops have already finished before the join begins:
	// drive the WaitGroup to zero and close both done channels.
	wg.Done()
	close(refreshDone)
	wg.Done()
	close(schedDone)

	returned := make(chan struct{})
	go func() {
		waitForBackgroundLoops(&wg, refreshDone, schedDone)
		close(returned)
	}()

	select {
	case <-returned:
	case <-time.After(2 * time.Second):
		t.Fatal("waitForBackgroundLoops did not return after both background loops completed")
	}
}

func TestWaitForBackgroundLoops_budgetExceeded_bothStuck(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	synctest.Test(t, func(t *testing.T) {
		var wg sync.WaitGroup
		wg.Add(2)
		refreshDone := make(chan struct{})
		schedDone := make(chan struct{})
		// Neither loop signals done, so both are still running when the
		// shutdown budget elapses; both must be named in still_running.
		waitForBackgroundLoops(&wg, refreshDone, schedDone)
		// Drain so the internal wg.Wait goroutine exits before the bubble ends.
		wg.Done()
		wg.Done()
	})

	out := buf.String()
	if !strings.Contains(out, "shutdown wait budget exceeded") {
		t.Errorf("expected budget-exceeded WARN, got: %q", out)
	}
	if !strings.Contains(out, "user-token-refresh") {
		t.Errorf("expected still_running to name user-token-refresh, got: %q", out)
	}
	if !strings.Contains(out, "scheduler") {
		t.Errorf("expected still_running to name scheduler, got: %q", out)
	}
}

func TestWaitForBackgroundLoops_budgetExceeded_onlySchedulerStuck(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	synctest.Test(t, func(t *testing.T) {
		var wg sync.WaitGroup
		wg.Add(2)
		refreshDone := make(chan struct{})
		schedDone := make(chan struct{})
		// Refresh loop finished; scheduler is the laggard, so only the
		// scheduler must appear in still_running.
		wg.Done()
		close(refreshDone)
		waitForBackgroundLoops(&wg, refreshDone, schedDone)
		wg.Done()
	})

	out := buf.String()
	if !strings.Contains(out, "shutdown wait budget exceeded") {
		t.Errorf("expected budget-exceeded WARN, got: %q", out)
	}
	if !strings.Contains(out, "scheduler") {
		t.Errorf("expected still_running to name scheduler, got: %q", out)
	}
	if strings.Contains(out, "user-token-refresh") {
		t.Errorf("still_running should not name user-token-refresh (it finished), got: %q", out)
	}
}

func TestNotifyAdapterGates_shortCircuitBeforeHTTP(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.WriteHeader(http.StatusNotFound)
	}))
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)

	build := func(play, scan bool) *notifyAdapter {
		c := cache.New()
		mgr := users.NewManager(c)
		mgr.Init(&plex.User{ID: "1", Name: "admin"})
		return &notifyAdapter{
			cfg:    &config{triggerOnPlay: play, triggerOnScan: scan},
			users:  mgr,
			client: plexclient.NewFromHTTP(base, "test-token", plexclient.Options{HTTP: srv.Client()}),
			cache:  c,
		}
	}

	relevantPlay := notify.PlayEvent{State: "playing", RatingKey: "1", ClientIdentifier: "mac-A"}
	relevantTimeline := []notify.TimelineEntry{{ItemID: "1", Type: 4, MetadataState: "created"}}
	ctx := t.Context()

	// Gates disabled: neither dispatch path may reach the Plex client.
	off := build(false, false)
	off.OnPlay(ctx, relevantPlay)
	off.OnTimeline(ctx, relevantTimeline)
	if n := hits.Load(); n != 0 {
		t.Fatalf("gates disabled but Plex client hit %d time(s); trigger-gate short-circuit regressed", n)
	}

	// Positive control: with the play gate enabled the same relevant event
	// reaches the Plex client (the session lookup 404s, so the fail-closed
	// resolve skips before any syncer call). Proves the counter is wired, so
	// the gate-disabled assertion above cannot pass vacuously.
	build(true, false).OnPlay(ctx, relevantPlay)
	if hits.Load() == 0 {
		t.Fatal("play gate enabled but Plex client was never hit; counter wiring is broken")
	}
}

// TestHandleTimeline_nonEpisodeNotMarked pins handleTimeline's mark-on-success
// ordering: MarkProcessed must run only AFTER the entry is confirmed a real,
// non-ignored episode, so an irrelevant/non-episode entry never suppresses a
// later genuine event for the same ItemID. It drives OnTimeline with a relevant
// entry whose fetched item is a non-episode (a movie) and asserts the cache key
// is NOT marked (the over-suppression failure mode).
func TestHandleTimeline_nonEpisodeNotMarked(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"1","type":"movie"}]}}`))
	}))
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)

	c := cache.New()
	mgr := users.NewManager(c)
	mgr.Init(&plex.User{ID: "1", Name: "admin"})
	adapter := &notifyAdapter{
		cfg:    &config{triggerOnScan: true},
		users:  mgr,
		client: plexclient.NewFromHTTP(base, "test-token", plexclient.Options{HTTP: srv.Client()}),
		cache:  c,
	}

	adapter.OnTimeline(t.Context(), []notify.TimelineEntry{{ItemID: "1", Type: 4, MetadataState: "created"}})

	if hits.Load() == 0 {
		t.Fatal("handleTimeline never fetched the item; it bailed before the type check, so the not-marked assertion is vacuous")
	}
	key := notify.BuildTimelineCacheKey("1")
	if c.WasRecentlyProcessed(key) {
		t.Error("handleTimeline marked a non-episode timeline entry processed; the mark-on-success ordering regressed (a later genuine event for the same ItemID would be suppressed)")
	}
	c.MarkProcessed(key)
	if !c.WasRecentlyProcessed(key) {
		t.Fatal("cache did not mark the timeline key; the not-marked assertion above would be vacuous")
	}
}

func TestWaitForBackgroundLoops_budgetExceeded_onlyRefreshStuck(t *testing.T) {
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, nil)))
	t.Cleanup(func() { slog.SetDefault(prev) })

	synctest.Test(t, func(t *testing.T) {
		var wg sync.WaitGroup
		wg.Add(2)
		refreshDone := make(chan struct{})
		schedDone := make(chan struct{})
		// Scheduler finished; the refresh loop is the laggard, so only the
		// refresh loop must appear in still_running.
		wg.Done()
		close(schedDone)
		waitForBackgroundLoops(&wg, refreshDone, schedDone)
		wg.Done()
	})

	out := buf.String()
	if !strings.Contains(out, "shutdown wait budget exceeded") {
		t.Errorf("expected budget-exceeded WARN, got: %q", out)
	}
	if !strings.Contains(out, "user-token-refresh") {
		t.Errorf("expected still_running to name user-token-refresh, got: %q", out)
	}
	if strings.Contains(out, "scheduler") {
		t.Errorf("still_running should not name scheduler (it finished), got: %q", out)
	}
}

type fakeIgnoreChecker struct{ skip bool }

func (f fakeIgnoreChecker) IgnoreLibrary(string) bool { return false }

func (f fakeIgnoreChecker) ShouldSkipEpisode(context.Context, api.PlexReader, *streams.Episode) bool {
	return f.skip
}

func TestHandleTimeline_ignoredEpisodeNotMarked(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"1","type":"episode"}]}}`))
	}))
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)

	c := cache.New()
	client := plexclient.NewFromHTTP(base, "test-token", plexclient.Options{HTTP: srv.Client()})
	mgr := users.NewManager(c)
	mgr.Init(&plex.User{ID: "1", Name: "admin"})
	syncer := tracksync.New(tracksync.Config{}, tracksync.Deps{Plex: client, Cache: c, Users: mgr, UserClient: func(string) api.PlexReadWriter { return nil }})
	adapter := &notifyAdapter{
		syncer: syncer,
		cfg:    &config{triggerOnScan: true},
		users:  mgr,
		client: client,
		cache:  c,
		ignore: fakeIgnoreChecker{skip: true},
	}

	adapter.OnTimeline(t.Context(), []notify.TimelineEntry{{ItemID: "1", Type: 4, MetadataState: "created"}})

	if hits.Load() == 0 {
		t.Fatal("handleTimeline never fetched the item; it bailed before the ignore check, so the not-marked assertion is vacuous")
	}
	key := notify.BuildTimelineCacheKey("1")
	if c.WasRecentlyProcessed(key) {
		t.Error("handleTimeline marked an ignored episode processed; the ignore gate (sole ignore enforcement for the scan/timeline path) regressed")
	}
	c.MarkProcessed(key)
	if !c.WasRecentlyProcessed(key) {
		t.Fatal("cache did not mark the timeline key; the not-marked assertion above would be vacuous")
	}
}

func TestHandleTimeline_genuineEpisodeMarkedAndDispatched(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"1","type":"episode"}]}}`))
	}))
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)

	c := cache.New()
	client := plexclient.NewFromHTTP(base, "test-token", plexclient.Options{HTTP: srv.Client()})
	mgr := users.NewManager(c)
	mgr.Init(&plex.User{ID: "1", Name: "admin"})
	syncer := tracksync.New(tracksync.Config{}, tracksync.Deps{Plex: client, Cache: c, Users: mgr, UserClient: func(string) api.PlexReadWriter { return nil }})
	adapter := &notifyAdapter{
		syncer: syncer,
		cfg:    &config{triggerOnScan: true},
		users:  mgr,
		client: client,
		cache:  c,
		// ignore nil: the n.ignore != nil conjunct is false, so a genuine
		// episode reaches the success tail (TimelineAction + MarkProcessed + dispatch).
	}

	adapter.OnTimeline(t.Context(), []notify.TimelineEntry{{ItemID: "1", Type: 4, MetadataState: "created"}})

	if hits.Load() == 0 {
		t.Fatal("handleTimeline never fetched the item; it bailed before the success tail, so the marked assertion is vacuous")
	}
	key := notify.BuildTimelineCacheKey("1")
	if !c.WasRecentlyProcessed(key) {
		t.Error("handleTimeline did not mark a genuine non-ignored episode processed; the success-path MarkProcessed regressed (duplicate timeline events for the same ItemID would no longer be deduped)")
	}
}

func TestHandleTimeline_alreadyProcessedSkipsRefetch(t *testing.T) {
	var hits atomic.Int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		hits.Add(1)
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[{"ratingKey":"1","type":"episode"}]}}`))
	}))
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)

	c := cache.New()
	client := plexclient.NewFromHTTP(base, "test-token", plexclient.Options{HTTP: srv.Client()})
	mgr := users.NewManager(c)
	mgr.Init(&plex.User{ID: "1", Name: "admin"})
	syncer := tracksync.New(tracksync.Config{}, tracksync.Deps{Plex: client, Cache: c, Users: mgr, UserClient: func(string) api.PlexReadWriter { return nil }})
	adapter := &notifyAdapter{
		syncer: syncer,
		cfg:    &config{triggerOnScan: true},
		users:  mgr,
		client: client,
		cache:  c,
	}
	entries := []notify.TimelineEntry{{ItemID: "1", Type: 4, MetadataState: "created"}}

	adapter.OnTimeline(t.Context(), entries)
	first := hits.Load()
	if first == 0 {
		t.Fatal("first timeline event never fetched the item; positive control broken, the dedup-skip assertion would be vacuous")
	}

	adapter.OnTimeline(t.Context(), entries)
	if hits.Load() != first {
		t.Errorf("re-fired timeline event for an already-processed ItemID hit Plex again (%d -> %d); the WasRecentlyProcessed dedup guard in handleTimeline regressed (a repeat event would re-run ProcessNewOrUpdatedEpisodeAllUsers)", first, hits.Load())
	}
}

func TestResolvePlayEventUser_sessionResolvesNonAdmin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[` +
			`{"User":{"id":"9","title":"bob"},"Player":{"machineIdentifier":"mac-B"}}]}}`))
	}))
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	adapter := &notifyAdapter{
		cfg:    &config{},
		client: plexclient.NewFromHTTP(base, "test-token", plexclient.Options{HTTP: srv.Client()}),
	}

	uid, uname, ok := adapter.resolvePlayEventUser(t.Context(),
		notify.PlayEvent{State: "playing", RatingKey: "100", ClientIdentifier: "mac-B"})

	if !ok || uid != "9" || uname != "bob" {
		t.Errorf("resolvePlayEventUser = (%q, %q, %v), want (9, bob, true); the fail-closed skip must NOT fire when the session resolves to a real user", uid, uname, ok)
	}
}

func TestResolvePlayEventUser_unresolvedSessionFailsClosed(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
	}))
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	adapter := &notifyAdapter{
		cfg:    &config{},
		client: plexclient.NewFromHTTP(base, "test-token", plexclient.Options{HTTP: srv.Client()}),
	}

	uid, uname, ok := adapter.resolvePlayEventUser(t.Context(),
		notify.PlayEvent{State: "playing", RatingKey: "100", ClientIdentifier: "mac-missing"})

	if ok {
		t.Errorf("resolvePlayEventUser ok = true for an unresolved session, got (%q, %q); an unresolved session must fail closed (skip), not fall back to admin", uid, uname)
	}
}

// ---------------------------------------------------------------------------
// Unattributed-play-event severity (resolveStallCounter)
// ---------------------------------------------------------------------------
//
// These pin the fix for a real production defect: the per-event skip was
// logged at WARN, which produced 163 operator-facing lines in 30 days for
// two expected, self-healing Plex behaviours, and buried the one state
// that matters (resolution failing across the board). The contract is a
// quiet Debug per event plus exactly one WARN per stall.

// captureLogs redirects the default logger at Debug level for the test
// and returns the buffer holding everything written to it.
func captureLogs(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	return &buf
}

func TestSkipUnattributedPlayEvent_singleSkipDoesNotWarn(t *testing.T) {
	buf := captureLogs(t)
	adapter := &notifyAdapter{cfg: &config{}, resolveStalls: &resolveStallCounter{}}

	adapter.skipUnattributedPlayEvent(
		notify.PlayEvent{State: "playing", RatingKey: "100", ClientIdentifier: "mac-A"},
		errUnattributedNoClient)

	out := buf.String()
	if strings.Contains(out, "level=WARN") {
		t.Errorf("a single unattributed play event logged at WARN: %q\nPlex announces playback up to 31s before the session is queryable and idle web clients re-announce finished items, so one skip is expected and self-healing; WARN here is the defect this change removed", out)
	}
	if !strings.Contains(out, "level=DEBUG") {
		t.Errorf("the skip left no DEBUG record, so a real stall would be undiagnosable: %q", out)
	}
	for _, want := range []string{"client=mac-A", "key=100", "state=playing"} {
		if !strings.Contains(out, want) {
			t.Errorf("skip record is missing %q, which the RCA needed to tell a start race from a stale client: %q", want, out)
		}
	}
}

func TestSkipUnattributedPlayEvent_warnsOnceAtStallThreshold(t *testing.T) {
	buf := captureLogs(t)
	adapter := &notifyAdapter{cfg: &config{}, resolveStalls: &resolveStallCounter{}}
	ev := notify.PlayEvent{State: "playing", RatingKey: "100", ClientIdentifier: "mac-A"}

	// Well past the threshold: a stall must cost one line, not one per
	// notification, or the fix trades 163 WARN lines for more.
	for range resolveStallThreshold * 3 {
		adapter.skipUnattributedPlayEvent(ev, errUnattributedNoClient)
	}

	if got := strings.Count(buf.String(), "level=WARN"); got != 1 {
		t.Errorf("WARN lines = %d, want exactly 1 for a single sustained stall; a per-notification WARN is the noise this change removed", got)
	}
	if !strings.Contains(buf.String(), "user resolution stalled") {
		t.Errorf("crossing the threshold did not report a stall, so total resolution failure is now silent: %q", buf.String())
	}
}

func TestResolveStallCounter_thresholdAndRecovery(t *testing.T) {
	c := &resolveStallCounter{}

	// One below the threshold must stay quiet.
	for i := 1; i < resolveStallThreshold; i++ {
		if stalled, n := c.miss(); stalled {
			t.Fatalf("miss %d of %d reported a stall; the expected races (longest measured run: 12) must never escalate", n, resolveStallThreshold)
		}
	}
	stalled, n := c.miss()
	if !stalled || n != resolveStallThreshold {
		t.Errorf("miss at the threshold = (%v, %d), want (true, %d)", stalled, n, resolveStallThreshold)
	}
	if again, _ := c.miss(); again {
		t.Error("a second stall was reported without an intervening success; the warn must fire once per stall")
	}

	recovered, after := c.success()
	if !recovered || after != resolveStallThreshold+1 {
		t.Errorf("success after a warned stall = (%v, %d), want (true, %d); recovery must be a positive log line, not the absence of one", recovered, after, resolveStallThreshold+1)
	}
	// The run is cleared, so the next stall must be able to fire again.
	for range resolveStallThreshold {
		stalled, _ = c.miss()
	}
	if !stalled {
		t.Error("the counter did not re-arm after recovery, so a second outage would be silent")
	}
}

func TestResolveStallCounter_successWithoutStallIsNotRecovery(t *testing.T) {
	c := &resolveStallCounter{}
	c.miss() // a normal race, well below the threshold

	if recovered, _ := c.success(); recovered {
		t.Error("a success after an ordinary sub-threshold race reported recovery; nothing warned, so nothing recovered")
	}
}

func TestResolveStallCounter_nilCountsNothing(t *testing.T) {
	// The resolver is exercised by tests that build no composition root,
	// so a nil counter must be inert rather than panic.
	var c *resolveStallCounter
	for range resolveStallThreshold + 1 {
		if stalled, _ := c.miss(); stalled {
			t.Fatal("a nil counter escalated")
		}
	}
	if recovered, _ := c.success(); recovered {
		t.Error("a nil counter reported recovery")
	}
}

func TestResolvePlayEventUser_unresolvedSessionCountsTowardStall(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
	}))
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	counter := &resolveStallCounter{}
	adapter := &notifyAdapter{
		cfg:           &config{},
		client:        plexclient.NewFromHTTP(base, "test-token", plexclient.Options{HTTP: srv.Client()}),
		resolveStalls: counter,
	}
	ev := notify.PlayEvent{State: "playing", RatingKey: "100", ClientIdentifier: "mac-missing"}

	for range resolveStallThreshold - 1 {
		adapter.resolvePlayEventUser(t.Context(), ev)
	}
	if stalled, n := counter.miss(); !stalled {
		t.Errorf("session-lookup failures did not accumulate toward the stall signal (run reached %d); both skip causes must count, or a broken resolver stays silent", n)
	}
}

func TestResolvePlayEventUser_successClearsTheStallRun(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[` +
			`{"User":{"id":"9","title":"bob"},"Player":{"machineIdentifier":"mac-B"}}]}}`))
	}))
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	counter := &resolveStallCounter{}
	adapter := &notifyAdapter{
		cfg:           &config{},
		client:        plexclient.NewFromHTTP(base, "test-token", plexclient.Options{HTTP: srv.Client()}),
		resolveStalls: counter,
	}
	// A start race: the event is unattributed, then the same client
	// resolves on a later notification, which is how production recovers.
	adapter.skipUnattributedPlayEvent(notify.PlayEvent{ClientIdentifier: "mac-B"}, errUnattributedNoClient)

	if _, _, ok := adapter.resolvePlayEventUser(t.Context(),
		notify.PlayEvent{State: "playing", RatingKey: "100", ClientIdentifier: "mac-B"}); !ok {
		t.Fatal("resolvePlayEventUser did not attribute a resolvable session")
	}
	if stalled, n := counter.miss(); stalled || n != 1 {
		t.Errorf("run after a success = (%v, %d), want (false, 1); a success must clear the run or transient races accumulate into a false stall", stalled, n)
	}
}

// ---------------------------------------------------------------------------
// isFatalStartupError / startupBackoff (degraded-start classifier + backoff)
// ---------------------------------------------------------------------------

func TestIsFatalStartupError(t *testing.T) {
	tests := []struct {
		name string
		err  error
		want bool
	}{
		{"bad token 401 is fatal", &plex.HTTPStatusError{Code: 401, Status: "401 Unauthorized", Method: "GET", Path: "/myplex/account"}, true},
		{"forbidden 403 is fatal", &plex.HTTPStatusError{Code: 403, Status: "403 Forbidden", Method: "GET", Path: "/"}, true},
		{"other 4xx is fatal", &plex.HTTPStatusError{Code: 400, Status: "400 Bad Request", Method: "GET", Path: "/"}, true},
		{"503 is transient", &plex.HTTPStatusError{Code: 503, Status: "503 Service Unavailable", Method: "GET", Path: "/"}, false},
		{"500 is transient", &plex.HTTPStatusError{Code: 500, Status: "500 Internal Server Error", Method: "GET", Path: "/"}, false},
		{"429 rate limited is transient", &plex.HTTPStatusError{Code: 429, Status: "429 Too Many Requests", Method: "GET", Path: "/"}, false},
		{"408 request timeout is transient", &plex.HTTPStatusError{Code: 408, Status: "408 Request Timeout", Method: "GET", Path: "/"}, false},
		{"not found (wrong server) is fatal", fmt.Errorf("connecting to plex server: %w", plex.ErrNotFound), true},
		{"unknown CA is fatal", fmt.Errorf("connecting to plex server: %w", &url.Error{Op: "Get", URL: "https://plex:32400/", Err: x509.UnknownAuthorityError{}}), true},
		{"cert verification error is fatal", fmt.Errorf("connecting to plex server: %w", &url.Error{Op: "Get", URL: "https://plex:32400/", Err: &tls.CertificateVerificationError{Err: errors.New("x509: certificate has expired or is not yet valid")}}), true},
		{"connection refused is transient", fmt.Errorf("connecting to plex server: %w", &url.Error{Op: "Get", URL: "http://127.0.0.1:1/", Err: errors.New("connect: connection refused")}), false},
		{"dns failure is transient", errors.New("connecting to plex server: dial tcp: lookup plex: no such host"), false},
		{"wrapped 401 is fatal", fmt.Errorf("resolving admin user: %w", &plex.HTTPStatusError{Code: 401, Status: "401 Unauthorized", Method: "GET", Path: "/myplex/account"}), true},
		{"nil is not fatal", nil, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := isFatalStartupError(tt.err); got != tt.want {
				t.Errorf("isFatalStartupError(%v) = %v, want %v", tt.err, got, tt.want)
			}
		})
	}
}

func TestStartupBackoff(t *testing.T) {
	if got := startupBackoff(0); got != startupBaseBackoff {
		t.Errorf("startupBackoff(0) = %v, want %v", got, startupBaseBackoff)
	}
	if got := startupBackoff(1); got != 2*startupBaseBackoff {
		t.Errorf("startupBackoff(1) = %v, want %v", got, 2*startupBaseBackoff)
	}
	// Large and negative attempt counts must cap cleanly, never overflowing the
	// duration to zero or a negative value.
	for _, n := range []int{5, 30, 62, 63, 100, -1} {
		if got := startupBackoff(n); got <= 0 || got > startupMaxBackoff {
			t.Errorf("startupBackoff(%d) = %v, want in (0, %v]", n, got, startupMaxBackoff)
		}
	}
}
