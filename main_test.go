// main_test.go holds tests for the composition-root concerns that
// remain in the main package after the cycle-1 extraction:
//
//   - Configuration loading (loadConfig + env helpers).
//   - Validation helpers (splitTrim, envBool, envOr, requireEnv with
//     _FILE secret handling, readSecretFile bounds) and the scheduler
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
//     tests → internal/sync/tracks_test.go.
//   - Scheduler worker-pool / dedup / circuit-breaker tests →
//     internal/scheduler/scheduler_test.go.
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
	syncpkg "github.com/cplieger/plex-language-sync/internal/sync"
	"github.com/cplieger/plex-language-sync/internal/users"
)

// ---------------------------------------------------------------------------
// envBool / envOr / splitTrim
// ---------------------------------------------------------------------------

func TestEnvBool(t *testing.T) {
	tests := []struct {
		val      string
		fallback bool
		want     bool
	}{
		{"true", false, true},
		{"1", false, true},
		{"yes", false, true},
		{"on", false, true},
		{"false", true, false},
		{"0", true, false},
		{"no", true, false},
		{"off", true, false},
		{"", true, true},
		{"invalid", true, true},
	}
	for _, tt := range tests {
		t.Setenv("TEST_ENV_BOOL", tt.val)
		if got := envBool("TEST_ENV_BOOL", tt.fallback); got != tt.want {
			t.Errorf("envBool(%q, %v) = %v, want %v", tt.val, tt.fallback, got, tt.want)
		}
	}
}

func TestEnvBoolCaseInsensitive(t *testing.T) {
	tests := []struct {
		val  string
		want bool
	}{
		{"TRUE", true},
		{"True", true},
		{"FALSE", false},
		{"False", false},
		{"YES", true},
		{"Yes", true},
		{"NO", false},
		{"No", false},
		{"ON", true},
		{"On", true},
		{"OFF", false},
		{"Off", false},
	}
	for _, tt := range tests {
		t.Setenv("TEST_ENV_BOOL_CI", tt.val)
		fallback := !tt.want
		if got := envBool("TEST_ENV_BOOL_CI", fallback); got != tt.want {
			t.Errorf("envBool(%q) = %v, want %v", tt.val, got, tt.want)
		}
	}
}

func TestEnvOr(t *testing.T) {
	t.Setenv("TEST_PLS_ENV", "custom")
	if got := envOr("TEST_PLS_ENV", "default"); got != "custom" {
		t.Errorf("envOr = %q, want custom", got)
	}
	t.Setenv("TEST_PLS_ENV", "")
	if got := envOr("TEST_PLS_ENV", "default"); got != "default" {
		t.Errorf("envOr = %q, want default", got)
	}
}

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

// ---------------------------------------------------------------------------
// readSecretFile bounds
// ---------------------------------------------------------------------------

func TestReadSecretFileOversized(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "big.txt")
	// Write just over the 1 MB limit.
	oversized := make([]byte, (1<<20)+1)
	for i := range oversized {
		oversized[i] = 'a'
	}
	if err := os.WriteFile(secretFile, oversized, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err := readSecretFile(secretFile)
	if err == nil {
		t.Error("readSecretFile should error on oversized file, got nil")
	}
	if !strings.Contains(err.Error(), "exceeds") {
		t.Errorf("error should mention size exceedance, got %v", err)
	}
}

func TestReadSecretFileNotFound(t *testing.T) {
	t.Parallel()
	_, err := readSecretFile("/nonexistent/path/secret.txt")
	if err == nil {
		t.Error("readSecretFile should error on missing file")
	}
}

func TestReadSecretFileValid(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "ok.txt")
	if err := os.WriteFile(secretFile, []byte("payload"), 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readSecretFile(secretFile)
	if err != nil {
		t.Fatalf("readSecretFile: %v", err)
	}
	if string(got) != "payload" {
		t.Errorf("readSecretFile = %q, want payload", got)
	}
}

func TestReadSecretFileExactLimit(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "exact.txt")
	data := make([]byte, 1<<20) // exactly 1 MB
	for i := range data {
		data[i] = 'x'
	}
	if err := os.WriteFile(secretFile, data, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readSecretFile(secretFile)
	if err != nil {
		t.Fatalf("readSecretFile at exact limit: %v", err)
	}
	if len(got) != 1<<20 {
		t.Errorf("readSecretFile len = %d, want %d", len(got), 1<<20)
	}
}

func TestReadSecretFilePathTraversal(t *testing.T) {
	t.Parallel()
	_, err := readSecretFile("/run/secrets/../../etc/passwd")
	if err == nil {
		t.Error("readSecretFile should reject path traversal")
	}
}

func TestReadSecretFileEmpty(t *testing.T) {
	t.Parallel()
	dir := t.TempDir()
	secretFile := filepath.Join(dir, "empty.txt")
	if err := os.WriteFile(secretFile, []byte{}, 0o600); err != nil {
		t.Fatal(err)
	}
	got, err := readSecretFile(secretFile)
	if err != nil {
		t.Fatalf("readSecretFile on empty file: unexpected error %v", err)
	}
	if len(got) != 0 {
		t.Errorf("readSecretFile on empty file = %q (len %d), want empty", got, len(got))
	}
}

// TestRequireEnvSecretFileEmptyDetection characterizes the empty-secret
// detection that requireEnv's _FILE branch performs after reading. The
// fail-fast itself (slog.Error + os.Exit(1)) cannot be exercised
// in-process and the repo intentionally has no subprocess/injected-exit
// harness, so this test pins the read-layer + trim contract requireEnv
// keys off: an empty OR whitespace-only secret file trims to "", which
// is what triggers the "secret file is empty" exit. If this contract
// regresses (e.g. readSecretFile starts erroring or trimming changes),
// the guard would silently stop firing. Mirrors the direct-env empty
// check (loadConfig requires PLEX_URL / PLEX_TOKEN non-empty); both the
// _FILE and direct branches must reject an effectively-empty value.
func TestRequireEnvSecretFileEmptyDetection(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		content string
	}{
		{"empty file", ""},
		{"whitespace only spaces", "   "},
		{"whitespace only newline", "\n"},
		{"whitespace tabs and newlines", "\t\n  \t\n"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			dir := t.TempDir()
			secretFile := filepath.Join(dir, "secret.txt")
			if err := os.WriteFile(secretFile, []byte(tt.content), 0o600); err != nil {
				t.Fatal(err)
			}
			data, err := readSecretFile(secretFile)
			if err != nil {
				t.Fatalf("readSecretFile(%q): unexpected error %v", tt.content, err)
			}
			// requireEnv's _FILE branch trims the bytes and fails fast
			// when the result is empty; assert the trim yields "" so the
			// guard fires for PLEX_TOKEN_FILE / PLEX_URL_FILE.
			if got := strings.TrimSpace(string(data)); got != "" {
				t.Errorf("TrimSpace(secret %q) = %q, want empty (would skip the empty-secret fail-fast)", tt.content, got)
			}
		})
	}
}

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

func newTestAdapter(t *testing.T, triggerOnPlay, triggerOnScan bool) notifyAdapter {
	t.Helper()
	parsed, _ := url.Parse("http://example.test")
	c := cache.New()
	client := plex.NewClientFromHTTP(parsed, "test-token", nil)
	mgr := users.NewManager(c)
	mgr.Init(&plex.User{ID: "1", Name: "admin"}, parsed, "")
	return notifyAdapter{
		syncer: nil, // unused on the gated-off paths
		cfg:    &config{triggerOnPlay: triggerOnPlay, triggerOnScan: triggerOnScan},
		users:  mgr,
		admin:  &plex.User{ID: "1", Name: "admin"},
		client: client,
		cache:  c,
	}
}

func TestResolvePlayEventUser_noClientIdentifier_returnsAdmin(t *testing.T) {
	adapter := newTestAdapter(t, true, false)
	ev := notify.PlayEvent{State: "playing", RatingKey: "100"}

	uid, uname := adapter.resolvePlayEventUser(context.Background(), ev)

	if uid != "1" {
		t.Errorf("resolvePlayEventUser userID = %q, want %q (admin fallback)", uid, "1")
	}
	if uname != "admin" {
		t.Errorf("resolvePlayEventUser username = %q, want %q (admin fallback)", uname, "admin")
	}
}

// NOTE: deep-dispatch tests for notifyAdapter.handlePlayEvent and
// handleTimeline (fetching episodes, dedup, ignored libraries, ignored
// shows, session resolution) live in internal/sync and internal/notify
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

	build := func(play, scan bool) notifyAdapter {
		c := cache.New()
		mgr := users.NewManager(c)
		mgr.Init(&plex.User{ID: "1", Name: "admin"}, base, "")
		return notifyAdapter{
			cfg:    &config{triggerOnPlay: play, triggerOnScan: scan},
			users:  mgr,
			admin:  &plex.User{ID: "1", Name: "admin"},
			client: plex.NewClientFromHTTP(base, "test-token", srv.Client()),
			cache:  c,
		}
	}

	relevantPlay := notify.PlayEvent{State: "playing", RatingKey: "1"}
	relevantTimeline := []notify.TimelineEntry{{ItemID: "1", Type: 4, MetadataState: "created"}}
	ctx := context.Background()

	// Gates disabled: neither dispatch path may reach the Plex client.
	off := build(false, false)
	off.OnPlay(ctx, relevantPlay)
	off.OnTimeline(ctx, relevantTimeline)
	if n := hits.Load(); n != 0 {
		t.Fatalf("gates disabled but Plex client hit %d time(s); trigger-gate short-circuit regressed", n)
	}

	// Positive control: with the play gate enabled the same relevant event
	// reaches the Plex client (the server 404s so handlePlayEvent returns at
	// ErrNotFound before any syncer call). Proves the counter is wired, so the
	// gate-disabled assertion above cannot pass vacuously.
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
	mgr.Init(&plex.User{ID: "1", Name: "admin"}, base, "")
	adapter := notifyAdapter{
		cfg:    &config{triggerOnScan: true},
		users:  mgr,
		admin:  &plex.User{ID: "1", Name: "admin"},
		client: plex.NewClientFromHTTP(base, "test-token", srv.Client()),
		cache:  c,
	}

	adapter.OnTimeline(context.Background(), []notify.TimelineEntry{{ItemID: "1", Type: 4, MetadataState: "created"}})

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
	client := plex.NewClientFromHTTP(base, "test-token", srv.Client())
	mgr := users.NewManager(c)
	mgr.Init(&plex.User{ID: "1", Name: "admin"}, base, "")
	syncer := syncpkg.NewSyncer(syncpkg.Config{}, client, c, mgr, func(string) api.PlexReadWriter { return nil })
	adapter := notifyAdapter{
		syncer: syncer,
		cfg:    &config{triggerOnScan: true},
		users:  mgr,
		admin:  &plex.User{ID: "1", Name: "admin"},
		client: client,
		cache:  c,
		ignore: fakeIgnoreChecker{skip: true},
	}

	adapter.OnTimeline(context.Background(), []notify.TimelineEntry{{ItemID: "1", Type: 4, MetadataState: "created"}})

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
	client := plex.NewClientFromHTTP(base, "test-token", srv.Client())
	mgr := users.NewManager(c)
	mgr.Init(&plex.User{ID: "1", Name: "admin"}, base, "")
	syncer := syncpkg.NewSyncer(syncpkg.Config{}, client, c, mgr, func(string) api.PlexReadWriter { return nil })
	adapter := notifyAdapter{
		syncer: syncer,
		cfg:    &config{triggerOnScan: true},
		users:  mgr,
		admin:  &plex.User{ID: "1", Name: "admin"},
		client: client,
		cache:  c,
		// ignore nil: the n.ignore != nil conjunct is false, so a genuine
		// episode reaches the success tail (TimelineAction + MarkProcessed + dispatch).
	}

	adapter.OnTimeline(context.Background(), []notify.TimelineEntry{{ItemID: "1", Type: 4, MetadataState: "created"}})

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
	client := plex.NewClientFromHTTP(base, "test-token", srv.Client())
	mgr := users.NewManager(c)
	mgr.Init(&plex.User{ID: "1", Name: "admin"}, base, "")
	syncer := syncpkg.NewSyncer(syncpkg.Config{}, client, c, mgr, func(string) api.PlexReadWriter { return nil })
	adapter := notifyAdapter{
		syncer: syncer,
		cfg:    &config{triggerOnScan: true},
		users:  mgr,
		admin:  &plex.User{ID: "1", Name: "admin"},
		client: client,
		cache:  c,
	}
	entries := []notify.TimelineEntry{{ItemID: "1", Type: 4, MetadataState: "created"}}

	adapter.OnTimeline(context.Background(), entries)
	first := hits.Load()
	if first == 0 {
		t.Fatal("first timeline event never fetched the item; positive control broken, the dedup-skip assertion would be vacuous")
	}

	adapter.OnTimeline(context.Background(), entries)
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
	adapter := notifyAdapter{
		cfg:    &config{},
		admin:  &plex.User{ID: "1", Name: "admin"},
		client: plex.NewClientFromHTTP(base, "test-token", srv.Client()),
	}

	uid, uname := adapter.resolvePlayEventUser(context.Background(),
		notify.PlayEvent{State: "playing", RatingKey: "100", ClientIdentifier: "mac-B"})

	if uid != "9" || uname != "bob" {
		t.Errorf("resolvePlayEventUser = (%q, %q), want (9, bob); the admin fallback must NOT fire when the session resolves to a real user", uid, uname)
	}
}

func TestResolvePlayEventUser_unresolvedSessionFallsBackAdmin(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"MediaContainer":{"Metadata":[]}}`))
	}))
	t.Cleanup(srv.Close)
	base, _ := url.Parse(srv.URL)
	adapter := notifyAdapter{
		cfg:    &config{},
		admin:  &plex.User{ID: "1", Name: "admin"},
		client: plex.NewClientFromHTTP(base, "test-token", srv.Client()),
	}

	uid, uname := adapter.resolvePlayEventUser(context.Background(),
		notify.PlayEvent{State: "playing", RatingKey: "100", ClientIdentifier: "mac-missing"})

	if uid != "1" || uname != "admin" {
		t.Errorf("resolvePlayEventUser = (%q, %q), want (1, admin); an unresolved session must fall back to admin", uid, uname)
	}
}
