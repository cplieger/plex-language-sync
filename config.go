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
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/cplieger/envx"
	syncpkg "github.com/cplieger/plex-language-sync/internal/sync"
	"github.com/cplieger/slogx"
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
	// Install the configured handler BEFORE the first envx read so a
	// malformed DEBUG value warns through it (logfmt, Loki-parseable) rather
	// than Go's pre-setup default logger; the level is then raised in place
	// once DEBUG is known. requireEnv errors get the same treatment.
	levelVar := slogx.Setup(slogx.Options{Level: slog.LevelInfo})
	debug := envx.Bool("DEBUG", false)
	if debug {
		levelVar.Set(slog.LevelDebug)
	}

	cfg := config{
		plexURL:          requireEnv("PLEX_URL"),
		plexToken:        requireEnv("PLEX_TOKEN"),
		updateLevel:      envx.String("UPDATE_LEVEL", defaultUpdateLevel),
		updateStrategy:   envx.String("UPDATE_STRATEGY", defaultUpdateStrategy),
		triggerOnPlay:    envx.Bool("TRIGGER_ON_PLAY", true),
		triggerOnScan:    envx.Bool("TRIGGER_ON_SCAN", true),
		languageProfiles: envx.Bool("LANGUAGE_PROFILES", true),
		debug:            debug,
		caCertPath:       envx.String("PLEX_CA_CERT_PATH", ""),
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

// requireEnv reads a required env var via envx.Secret, which also supports
// the Docker-secrets convention (KEY_FILE pointing at a mounted file,
// size-bounded, trimmed). Missing or unreadable values are fatal: the
// process cannot work without them, and exiting through the configured
// slog handler keeps the failure loud and greppable.
func requireEnv(key string) string {
	v, err := envx.Secret(key)
	if err != nil {
		slog.Error("required environment variable is missing or unreadable", "key", key, "error", err)
		os.Exit(1)
	}
	return v
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
