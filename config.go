// config.go owns application configuration and env-var parsing for
// the composition root.
//
// Everything in this file is main-package state that the wiring in
// run() reads at startup. The env-var contract (names, defaults,
// boolean parsing, _FILE secret handling, HH:MM parsing) is frozen
// per the refactor inviolate contract (item 3); in-memory
// representation may evolve freely.

package main

import (
	"fmt"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"strings"

	syncpkg "plex-language-sync/internal/sync"
	"plex-language-sync/internal/timeutil"
)

// ---------------------------------------------------------------------------
// Defaults
// ---------------------------------------------------------------------------

const (
	defaultUpdateLevel    = syncpkg.LevelShow
	defaultUpdateStrategy = syncpkg.StrategyAll
	defaultScheduleTime   = "02:00"
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
	plexURL             string
	plexToken           string
	updateLevel         string // "show" or "season"
	updateStrategy      string // "all" or "next"
	schedulerTime       string // "HH:MM"
	caCertPath          string
	ignoreLabels        []string
	ignoreLibraries     []string
	triggerOnPlay       bool
	triggerOnScan       bool
	schedulerEnable     bool
	languageProfiles    bool
	debug               bool
}

// loadConfig reads environment variables into a config value, applying
// the frozen defaults and validation rules. On missing required vars it
// emits slog.Error and terminates the process via os.Exit(1).
func loadConfig() config {
	// Set up slog handler early so requireEnv errors use the configured handler.
	debug := envBool("DEBUG", false)
	level := slog.LevelInfo
	if debug {
		level = slog.LevelDebug
	}
	slog.SetDefault(slog.New(slog.NewTextHandler(os.Stderr,
		&slog.HandlerOptions{Level: level})))

	cfg := config{
		plexURL:             requireEnv("PLEX_URL"),
		plexToken:           requireEnv("PLEX_TOKEN"),
		updateLevel:         envOr("UPDATE_LEVEL", defaultUpdateLevel),
		updateStrategy:      envOr("UPDATE_STRATEGY", defaultUpdateStrategy),
		triggerOnPlay:       envBool("TRIGGER_ON_PLAY", true),
		triggerOnScan:       envBool("TRIGGER_ON_SCAN", true),
		schedulerEnable:     envBool("SCHEDULER_ENABLE", true),
		languageProfiles:    envBool("LANGUAGE_PROFILES", true),
		schedulerTime:       envOr("SCHEDULER_SCHEDULE_TIME", defaultScheduleTime),
		debug:               debug,
		caCertPath:          envOr("PLEX_CA_CERT_PATH", ""),
	}

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

	if valid := validateScheduleTime(cfg.schedulerTime); valid != cfg.schedulerTime {
		slog.Warn("invalid SCHEDULER_SCHEDULE_TIME, defaulting", "value", cfg.schedulerTime)
		cfg.schedulerTime = valid
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
		"scheduler_enable", cfg.schedulerEnable,
		"language_profiles", cfg.languageProfiles,
		"scheduler_time", cfg.schedulerTime,
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
		return strings.TrimSpace(string(data))
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
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

func envBool(key string, fallback bool) bool {
	v := strings.TrimSpace(os.Getenv(key))
	if v == "" {
		return fallback
	}
	switch strings.ToLower(v) {
	case "true", "1", "yes":
		return true
	case "false", "0", "no":
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

// validateScheduleTime returns the time string if valid, or the default.
// Delegates HH:MM parsing to internal/timeutil so config and scheduler
// share the same implementation.
func validateScheduleTime(raw string) string {
	if _, _, ok := timeutil.ParseHHMM(raw); !ok {
		return defaultScheduleTime
	}
	return raw
}
