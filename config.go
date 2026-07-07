// config.go owns application configuration and env-var parsing for
// the composition root.
//
// Everything in this file is main-package state that the wiring in
// run() reads at startup. The env-var contract (names, defaults,
// boolean parsing, _FILE secret handling, Go-duration SCHEDULER_INTERVAL
// parsing) is stable; the in-memory representation may evolve freely.
// The former frozen HH:MM SCHEDULER_SCHEDULE_TIME contract was
// deliberately replaced by the fleet-standard SCHEDULER_INTERVAL (a Go
// duration) so the app no longer reads local wall-clock time — see the
// scheduler package.

package main

import (
	"cmp"
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"time"

	syncpkg "github.com/cplieger/plex-language-sync/internal/sync"
)

// ---------------------------------------------------------------------------
// Defaults
// ---------------------------------------------------------------------------

const (
	defaultUpdateLevel       = syncpkg.LevelShow
	defaultUpdateStrategy    = syncpkg.StrategyAll
	defaultSchedulerInterval = 24 * time.Hour
)

// Default ignore labels applied when IGNORE_LABELS is not set.
const (
	labelPALIgnore = "PAL_IGNORE"
	labelPLSIgnore = "PLS_IGNORE"
)

// ---------------------------------------------------------------------------
// Configuration
// ---------------------------------------------------------------------------

type config struct {
	plexURL           string
	plexToken         string
	updateLevel       string // "show" or "season"
	updateStrategy    string // "all" or "next"
	caCertPath        string
	ignoreLabels      []string
	ignoreLibraries   []string
	schedulerInterval time.Duration // deep-analysis cadence; 0 = disabled
	triggerOnPlay     bool
	triggerOnScan     bool
	schedulerEnabled  bool
	languageProfiles  bool
	debug             bool
}

// loadConfig reads environment variables into a config value, applying
// the defaults and validation rules. On missing required vars it emits
// slog.Error and terminates the process via os.Exit(1).
func loadConfig() config {
	// Set up slog handler early so requireEnv errors use the configured handler.
	debug := envBool("DEBUG", false)
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: level, ReplaceAttr: utcTimeAttr})))

	cfg := config{
		plexURL:          requireEnv("PLEX_URL"),
		plexToken:        requireEnv("PLEX_TOKEN"),
		updateLevel:      envOr("UPDATE_LEVEL", defaultUpdateLevel),
		updateStrategy:   envOr("UPDATE_STRATEGY", defaultUpdateStrategy),
		triggerOnPlay:    envBool("TRIGGER_ON_PLAY", true),
		triggerOnScan:    envBool("TRIGGER_ON_SCAN", true),
		languageProfiles: envBool("LANGUAGE_PROFILES", true),
		debug:            debug,
		caCertPath:       envOr("PLEX_CA_CERT_PATH", ""),
	}
	cfg.schedulerInterval, cfg.schedulerEnabled = loadSchedulerInterval()

	if v := os.Getenv("IGNORE_LABELS"); v != "" {
		cfg.ignoreLabels = splitTrim(v)
	} else {
		cfg.ignoreLabels = []string{labelPALIgnore, labelPLSIgnore}
	}
	if v := os.Getenv("IGNORE_LIBRARIES"); v != "" {
		cfg.ignoreLibraries = splitTrim(v)
	}

	if cfg.updateLevel != syncpkg.LevelShow && cfg.updateLevel != syncpkg.LevelSeason {
		slog.Warn("invalid UPDATE_LEVEL, defaulting to show", "value", cfg.updateLevel)
		cfg.updateLevel = defaultUpdateLevel
	}
	if cfg.updateStrategy != syncpkg.StrategyAll && cfg.updateStrategy != syncpkg.StrategyNext {
		slog.Warn("invalid UPDATE_STRATEGY, defaulting to all", "value", cfg.updateStrategy)
		cfg.updateStrategy = defaultUpdateStrategy
	}

	return cfg
}

// logConfig emits the loaded configuration at INFO. The plex_token field
// is deliberately logged as "configured" rather than its real value.
func logConfig(cfg *config) {
	slog.Info("configuration loaded",
		"plex_url", cfg.plexURL,
		"plex_token", "configured",
		"update_level", cfg.updateLevel,
		"update_strategy", cfg.updateStrategy,
		"trigger_on_play", cfg.triggerOnPlay,
		"trigger_on_scan", cfg.triggerOnScan,
		"scheduler_enabled", cfg.schedulerEnabled,
		"language_profiles", cfg.languageProfiles,
		"scheduler_interval", cfg.schedulerInterval.String(),
		"ignore_labels", cfg.ignoreLabels,
		"ignore_libraries", cfg.ignoreLibraries,
		"debug", cfg.debug,
		"ca_cert_path", cfg.caCertPath)
}

// ---------------------------------------------------------------------------
// Environment helpers
// ---------------------------------------------------------------------------

// requireEnv returns the value of key, with _FILE-suffix Docker-secret
// handling. Missing values terminate the process.
func requireEnv(key string) string {
	// Support Docker secrets via _FILE suffix.
	if filePath := os.Getenv(key + "_FILE"); filePath != "" {
		data, err := readSecretFile(filePath)
		if err != nil {
			slog.Error("cannot read secret file", "key", key+"_FILE", "path", filePath, "error", err)
			os.Exit(1)
		}
		v := strings.TrimSpace(string(data))
		if v == "" {
			slog.Error("secret file is empty", "key", key+"_FILE", "path", filePath)
			os.Exit(1)
		}
		return v
	}
	v := os.Getenv(key)
	if v == "" {
		slog.Error("required environment variable is missing", "key", key)
		os.Exit(1)
	}
	return v
}

// readSecretFile reads a secret file with size validation using a single file
// handle to avoid TOCTOU races between stat and read.
func readSecretFile(filePath string) ([]byte, error) {
	const maxSecretSize = 1 << 20 // 1 MB
	cleaned := filepath.Clean(filePath)
	if cleaned != filePath || strings.Contains(filePath, "..") {
		return nil, fmt.Errorf("path traversal detected in secret file path: %s", filePath)
	}
	f, err := os.Open(cleaned)
	if err != nil {
		return nil, err
	}
	defer f.Close()
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if info.Size() > maxSecretSize {
		return nil, fmt.Errorf("file is %d bytes, exceeds %d byte limit", info.Size(), maxSecretSize)
	}
	return io.ReadAll(io.LimitReader(f, maxSecretSize+1))
}

func envOr(key, fallback string) string {
	return cmp.Or(os.Getenv(key), fallback)
}

func envBool(key string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	switch strings.ToLower(v) {
	case "true", "1", "yes", "on":
		return true
	case "false", "0", "no", "off":
		return false
	default:
		slog.Warn("unrecognized boolean value, using default",
			"key", key, "value", v, "default", fallback)
		return fallback
	}
}

func splitTrim(s string) []string {
	parts := strings.Split(s, ",")
	out := make([]string, 0, len(parts))
	for _, p := range parts {
		p = strings.TrimSpace(p)
		if p != "" {
			out = append(out, p)
		}
	}
	return out
}

// loadSchedulerInterval parses SCHEDULER_INTERVAL and reports the daily
// deep-analysis cadence and whether the scheduler runs at all. The value
// is a Go duration ("24h", "12h"), matching the fleet docker-*-scheduler
// convention. The sentinels "off" and "disabled" (case-insensitive) or a
// zero duration ("0", "0s") disable the scheduler entirely: the app then
// runs WebSocket-only (the daily pass is a safety net over the real-time
// listener, and there is no external trigger). Unset defaults to
// defaultSchedulerInterval, enabled. Any other parse failure falls back
// to the default (enabled) with a warning rather than refusing to start.
func loadSchedulerInterval() (interval time.Duration, enabled bool) {
	interval = defaultSchedulerInterval
	enabled = true
	raw := strings.TrimSpace(os.Getenv("SCHEDULER_INTERVAL"))
	if raw == "" {
		return interval, enabled
	}
	if lower := strings.ToLower(raw); lower == "off" || lower == "disabled" {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	switch {
	case err != nil:
		slog.Warn("cannot parse SCHEDULER_INTERVAL, using default",
			"value", raw, "default", defaultSchedulerInterval.String())
	case d == 0:
		// "0"/"0s" disables the daily safety-net pass.
		return 0, false
	case d < 0:
		// A negative duration is a likely typo, not a documented disable
		// sentinel (off/disabled/0/0s); warn and fall back to the default
		// rather than silently idling.
		slog.Warn("SCHEDULER_INTERVAL is negative, using default",
			"value", raw, "default", defaultSchedulerInterval.String())
	default:
		interval = d
	}
	return interval, enabled
}

// utcTimeAttr is a slog ReplaceAttr that renders the record's built-in time
// key in UTC, so log-line timestamps are zone-stable regardless of the
// container's TZ (the fleet logs-in-UTC standard). It rewrites only the
// top-level time attribute; a user attribute that happens to share the "time"
// key inside a group is left untouched.
func utcTimeAttr(groups []string, a slog.Attr) slog.Attr {
	if len(groups) == 0 && a.Key == slog.TimeKey && a.Value.Kind() == slog.KindTime {
		a.Value = slog.TimeValue(a.Value.Time().UTC())
	}
	return a
}
