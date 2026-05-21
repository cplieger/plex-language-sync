// main_test.go holds tests for the composition-root concerns that
// remain in the main package after the cycle-1 extraction:
//
//   - Configuration loading (loadConfig + env helpers).
//   - Validation helpers (validateScheduleTime, splitTrim, envBool,
//     envOr, requireEnv with _FILE secret handling, readSecretFile
//     bounds). The HH:MM parsing itself lives in internal/timeutil
//     and is covered there; this file only exercises the config-side
//     wrapper (validateScheduleTime).
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
	"context"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"plex-language-sync/internal/cache"
	"plex-language-sync/internal/notify"
	"plex-language-sync/internal/plex"
	"plex-language-sync/internal/users"
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
		{"false", true, false},
		{"0", true, false},
		{"no", true, false},
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
// validateScheduleTime (parseHHMM coverage lives in internal/timeutil)
// ---------------------------------------------------------------------------

func TestValidateScheduleTime(t *testing.T) {
	tests := []struct {
		input string
		want  string
	}{
		{"02:00", "02:00"},
		{"23:59", "23:59"},
		{"00:00", "00:00"},
		{"invalid", "02:00"},
		{"25:00", "02:00"},
		{"", "02:00"},
	}
	for _, tt := range tests {
		got := validateScheduleTime(tt.input)
		if got != tt.want {
			t.Errorf("validateScheduleTime(%q) = %q, want %q", tt.input, got, tt.want)
		}
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
	t.Setenv("SCHEDULER_ENABLE", "true")
	t.Setenv("LANGUAGE_PROFILES", "false")
	t.Setenv("SCHEDULER_SCHEDULE_TIME", "03:00")
	t.Setenv("IGNORE_LABELS", "SKIP,NOPE")
	t.Setenv("IGNORE_LIBRARIES", "Music,Photos")
	t.Setenv("DEBUG", "false")
	t.Setenv("SKIP_TLS_VERIFICATION", "false")

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
	if cfg.schedulerTime != "03:00" {
		t.Errorf("schedulerTime = %q, want 03:00", cfg.schedulerTime)
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
	t.Setenv("SCHEDULER_ENABLE", "")
	t.Setenv("LANGUAGE_PROFILES", "")
	t.Setenv("SCHEDULER_SCHEDULE_TIME", "")
	t.Setenv("IGNORE_LABELS", "")
	t.Setenv("IGNORE_LIBRARIES", "")
	t.Setenv("DEBUG", "")
	t.Setenv("SKIP_TLS_VERIFICATION", "")
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
	if len(cfg.ignoreLabels) != 2 {
		t.Errorf("ignoreLabels should default to 2 items, got %v", cfg.ignoreLabels)
	}
}

func TestLoadConfigInvalidUpdateLevel(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("PLEX_TOKEN", "test-token")
	t.Setenv("UPDATE_LEVEL", "invalid")
	t.Setenv("UPDATE_STRATEGY", "invalid")
	t.Setenv("SCHEDULER_SCHEDULE_TIME", "25:99")
	t.Setenv("PLEX_URL_FILE", "")
	t.Setenv("PLEX_TOKEN_FILE", "")
	t.Setenv("IGNORE_LABELS", "")
	t.Setenv("IGNORE_LIBRARIES", "")
	t.Setenv("DEBUG", "")
	t.Setenv("SKIP_TLS_VERIFICATION", "")

	cfg := loadConfig()

	if cfg.updateLevel != "show" {
		t.Errorf("invalid updateLevel should default to show, got %q", cfg.updateLevel)
	}
	if cfg.updateStrategy != "all" {
		t.Errorf("invalid updateStrategy should default to all, got %q", cfg.updateStrategy)
	}
	if cfg.schedulerTime != "02:00" {
		t.Errorf("invalid schedulerTime should default to 02:00, got %q", cfg.schedulerTime)
	}
}

func TestLoadConfigInvalidUpdateStrategy(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("PLEX_TOKEN", "test-token")
	t.Setenv("UPDATE_STRATEGY", "random")
	t.Setenv("UPDATE_LEVEL", "")
	t.Setenv("SCHEDULER_SCHEDULE_TIME", "")
	t.Setenv("PLEX_URL_FILE", "")
	t.Setenv("PLEX_TOKEN_FILE", "")
	t.Setenv("IGNORE_LABELS", "")
	t.Setenv("IGNORE_LIBRARIES", "")
	t.Setenv("DEBUG", "")
	t.Setenv("SKIP_TLS_VERIFICATION", "")

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
	t.Setenv("SCHEDULER_SCHEDULE_TIME", "")
	t.Setenv("PLEX_URL_FILE", "")
	t.Setenv("PLEX_TOKEN_FILE", "")
	t.Setenv("IGNORE_LABELS", "")
	t.Setenv("IGNORE_LIBRARIES", "")
	t.Setenv("DEBUG", "")
	t.Setenv("SKIP_TLS_VERIFICATION", "")

	cfg := loadConfig()
	if cfg.updateStrategy != "next" {
		t.Errorf("expected updateStrategy=next, got %q", cfg.updateStrategy)
	}
}

func TestLoadConfigSchedulerTimeOutOfRange(t *testing.T) {
	tests := []struct {
		name string
		val  string
	}{
		{"hour 24", "24:00"},
		{"minute 60", "23:60"},
		{"both out", "25:99"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Setenv("PLEX_URL", "http://plex:32400")
			t.Setenv("PLEX_TOKEN", "test-token")
			t.Setenv("SCHEDULER_SCHEDULE_TIME", tt.val)
			t.Setenv("UPDATE_LEVEL", "")
			t.Setenv("UPDATE_STRATEGY", "")
			t.Setenv("PLEX_URL_FILE", "")
			t.Setenv("PLEX_TOKEN_FILE", "")
			t.Setenv("IGNORE_LABELS", "")
			t.Setenv("IGNORE_LIBRARIES", "")
			t.Setenv("DEBUG", "")
			t.Setenv("SKIP_TLS_VERIFICATION", "")
			cfg := loadConfig()
			if cfg.schedulerTime != "02:00" {
				t.Errorf("%s: schedulerTime = %q, want 02:00", tt.val, cfg.schedulerTime)
			}
		})
	}
}

func TestLoadConfigSchedulerTimeNoColon(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("PLEX_TOKEN", "test-token")
	t.Setenv("SCHEDULER_SCHEDULE_TIME", "0230")
	t.Setenv("UPDATE_LEVEL", "")
	t.Setenv("UPDATE_STRATEGY", "")
	t.Setenv("PLEX_URL_FILE", "")
	t.Setenv("PLEX_TOKEN_FILE", "")
	t.Setenv("IGNORE_LABELS", "")
	t.Setenv("IGNORE_LIBRARIES", "")
	t.Setenv("DEBUG", "")
	t.Setenv("SKIP_TLS_VERIFICATION", "")

	cfg := loadConfig()
	if cfg.schedulerTime != "02:00" {
		t.Errorf("schedulerTime without colon should default to 02:00, got %q", cfg.schedulerTime)
	}
}

func TestLoadConfigDebugMode(t *testing.T) {
	t.Setenv("PLEX_URL", "http://plex:32400")
	t.Setenv("PLEX_TOKEN", "test-token")
	t.Setenv("UPDATE_LEVEL", "")
	t.Setenv("UPDATE_STRATEGY", "")
	t.Setenv("TRIGGER_ON_PLAY", "")
	t.Setenv("TRIGGER_ON_SCAN", "")
	t.Setenv("SCHEDULER_ENABLE", "")
	t.Setenv("LANGUAGE_PROFILES", "")
	t.Setenv("SCHEDULER_SCHEDULE_TIME", "")
	t.Setenv("IGNORE_LABELS", "")
	t.Setenv("IGNORE_LIBRARIES", "")
	t.Setenv("DEBUG", "true")
	t.Setenv("SKIP_TLS_VERIFICATION", "")
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
	t.Setenv("SCHEDULER_ENABLE", "")
	t.Setenv("LANGUAGE_PROFILES", "")
	t.Setenv("SCHEDULER_SCHEDULE_TIME", "")
	t.Setenv("IGNORE_LABELS", "")
	t.Setenv("IGNORE_LIBRARIES", "")
	t.Setenv("DEBUG", "")
	t.Setenv("SKIP_TLS_VERIFICATION", "")

	cfg := loadConfig()

	if cfg.plexURL != "http://plex:32400" {
		t.Errorf("plexURL = %q, want http://plex:32400", cfg.plexURL)
	}
	if cfg.plexToken != "secret-token" {
		t.Errorf("plexToken = %q, want secret-token", cfg.plexToken)
	}
}

func TestLogConfig(t *testing.T) {
	cfg := &config{
		plexURL:        "http://plex:32400",
		plexToken:      "test-token",
		updateLevel:    "show",
		updateStrategy: "all",
		schedulerTime:  "02:00",
		ignoreLabels:   []string{"SKIP"},
	}
	logConfig(cfg)
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
	mgr.Init(&plex.User{ID: "1", Name: "admin"}, parsed, false)
	return notifyAdapter{
		syncer: nil, // unused on the gated-off paths
		cfg:    &config{triggerOnPlay: triggerOnPlay, triggerOnScan: triggerOnScan},
		users:  mgr,
		admin:  &plex.User{ID: "1", Name: "admin"},
		client: client,
		cache:  c,
	}
}

func TestNotifyAdapterTriggersDisabled(t *testing.T) {
	adapter := newTestAdapter(t, false, false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()

	// Both paths must return without touching any collaborator when the
	// gate is off — an empty-event slice is a lenient probe and the
	// adapter should exit before dereferencing syncer.
	adapter.OnPlay(ctx, notify.PlayEvent{State: "playing", RatingKey: "1"})
	adapter.OnTimeline(ctx, []notify.TimelineEntry{{ItemID: "1", Type: 4, State: 5}})
}

func TestNotifyAdapterPlayEnabledEmptyEvents(t *testing.T) {
	adapter := newTestAdapter(t, true, false)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// A non-relevant state returns early inside handlePlayEvent (before
	// any syncer call) — the test passes if no panic occurs.
	adapter.OnPlay(ctx, notify.PlayEvent{State: "stopped", RatingKey: "1"})
}

func TestNotifyAdapterTimelineEnabledEmptyEntries(t *testing.T) {
	adapter := newTestAdapter(t, false, true)
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// Empty slice → no loop iterations.
	adapter.OnTimeline(ctx, nil)
}

// NOTE: deep-dispatch tests for notifyAdapter.handlePlayEvent and
// handleTimeline (fetching episodes, dedup, ignored libraries, ignored
// shows, session resolution) live in internal/sync and internal/notify
// now — the per-feature logic moved out of the main package in
// cycle-1 steps 5 and 7. What remains here is the trigger-gate
// behaviour, which is a composition-root concern.
