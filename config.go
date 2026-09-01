// config.go owns application configuration and env-var parsing for
// the composition root.
//
// The env-var contract (names, defaults, boolean parsing, _FILE secret
// handling, Go-duration DEEP_SCAN_INTERVAL parsing) is stable; the
// in-memory representation may evolve freely.

package main

import (
	"cmp"
	"log/slog"
	"os"
	"strings"
	"time"

	"github.com/cplieger/envx/v2"
	"github.com/cplieger/langtag/v2"
	"github.com/cplieger/plex-language-sync/internal/tracksync"
	"github.com/cplieger/slogx"
)

// ---------------------------------------------------------------------------
// Defaults
// ---------------------------------------------------------------------------

const (
	defaultUpdateLevel       = tracksync.LevelShow
	defaultUpdateStrategy    = tracksync.StrategyAll
	defaultSchedulerInterval = 24 * time.Hour
)

// defaultSubtitleTier is the furthest language distance a subtitle substitution
// reaches unless SUBTITLE_MATCH_TIER says otherwise.
//
// Same-language, which pairs with the fixed audio floor to mean one thing on
// both paths: never a different language. The tier numbers differ only because
// a spoken track has no script, so the audio path has to absorb a script
// difference that is an artifact of inferring one from the region.
//
// It stops one tier short of what looks like the last judgment-free rung.
// Crossing a script boundary is judgment-free but it is not free for the
// reader: CLDR rates a generic cross-script substitution at distance 50, where
// every curated close-language pair is between 4 and 20, and there is no
// Simplified-to-Traditional Chinese entry at all so Chinese falls to that
// generic 50. Handing a Simplified reader Traditional subtitles across a whole
// show is a bigger imposition than any tier above this one, so it is opt-in.
const defaultSubtitleTier = langtag.TierSameLanguage

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
	logLevel          slog.Level    // level installed from LOG_LEVEL
	subtitleTier      langtag.Tier  // furthest language distance for a subtitle match
	triggerOnPlay     bool
	triggerOnScan     bool
	schedulerEnabled  bool
	languageProfiles  bool
}

// loadConfig reads environment variables into a config value, applying
// the defaults and validation rules. On missing required vars it emits
// slog.Error and terminates the process via os.Exit(1).
func loadConfig() config {
	// Install the configured handler BEFORE the first envx read so a
	// malformed value warns through it rather than Go's pre-setup default
	// logger; the level is set once LOG_LEVEL is parsed. slogx.ParseLevel
	// returns ok rather than logging so this call can put its own warning
	// on the configured handler too.
	levelVar := slogx.Setup(slogx.Options{Level: slog.LevelInfo})
	rawLevel := envx.String("LOG_LEVEL")
	logLevel, recognized := slogx.ParseLevel(rawLevel, slog.LevelInfo)
	if !recognized {
		slog.Warn("invalid LOG_LEVEL, using default", "value", rawLevel, "default", "info")
	}
	levelVar.Set(logLevel)

	cfg := config{
		plexURL:          requireEnv("PLEX_URL"),
		plexToken:        requireEnv("PLEX_TOKEN"),
		updateLevel:      cmp.Or(envx.String("UPDATE_LEVEL"), defaultUpdateLevel),
		updateStrategy:   cmp.Or(envx.String("UPDATE_STRATEGY"), defaultUpdateStrategy),
		triggerOnPlay:    envx.Bool("TRIGGER_ON_PLAY", true),
		triggerOnScan:    envx.Bool("TRIGGER_ON_SCAN", true),
		languageProfiles: envx.Bool("LEARN_LANGUAGE_PROFILES", true),
		logLevel:         logLevel,
		caCertPath:       envx.String("PLEX_CA_CERT_PATH"),
		subtitleTier:     loadSubtitleTier(),
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

	if cfg.updateLevel != tracksync.LevelShow && cfg.updateLevel != tracksync.LevelSeason {
		slog.Warn("invalid UPDATE_LEVEL, defaulting to show", "value", cfg.updateLevel)
		cfg.updateLevel = defaultUpdateLevel
	}
	if cfg.updateStrategy != tracksync.StrategyAll && cfg.updateStrategy != tracksync.StrategyNext {
		slog.Warn("invalid UPDATE_STRATEGY, defaulting to all", "value", cfg.updateStrategy)
		cfg.updateStrategy = defaultUpdateStrategy
	}

	return cfg
}

// loadSubtitleTier parses SUBTITLE_MATCH_TIER, warning and falling back to the
// default on an unrecognized value, which is how UPDATE_LEVEL and
// UPDATE_STRATEGY already behave.
//
// The fallback matters more than it looks. langtag.ParseTier returns TierNone
// for input it does not recognize, and TierNone as a floor means "no
// relationship", so a typo must not be passed through: the library refuses such
// a floor outright, and this app substitutes the documented default so a
// mistyped value degrades to normal behavior rather than to no matching at all.
func loadSubtitleTier() langtag.Tier {
	raw := envx.String("SUBTITLE_MATCH_TIER")
	if raw == "" {
		return defaultSubtitleTier
	}
	tier, ok := langtag.ParseTier(raw)
	if !ok {
		slog.Warn("cannot parse SUBTITLE_MATCH_TIER, using default",
			"value", raw, "default", defaultSubtitleTier.String())
		return defaultSubtitleTier
	}
	return tier
}

// logConfig emits the loaded configuration at INFO. The plex_token field
// is deliberately logged as "configured" rather than its real value.
func logConfig(cfg *config) {
	slog.Info("configuration loaded",
		"plex_url", cfg.plexURL,
		"plex_token", "configured",
		"update_level", cfg.updateLevel,
		"update_strategy", cfg.updateStrategy,
		"subtitle_match_tier", cfg.subtitleTier.String(),
		"trigger_on_play", cfg.triggerOnPlay,
		"trigger_on_scan", cfg.triggerOnScan,
		"scheduler_enabled", cfg.schedulerEnabled,
		"learn_language_profiles", cfg.languageProfiles,
		"deep_scan_interval", cfg.schedulerInterval.String(),
		"ignore_labels", cfg.ignoreLabels,
		"ignore_libraries", cfg.ignoreLibraries,
		"log_level", cfg.logLevel.String(),
		"ca_cert_path", cfg.caCertPath)
}

// ---------------------------------------------------------------------------
// Environment helpers
// ---------------------------------------------------------------------------

// requireEnv reads a required env var via envx.Secret, which also supports
// the Docker-secrets convention (KEY_FILE pointing at a mounted file,
// size-bounded), and then trims the result.
//
// envx returns a KEY_FILE value verbatim except for one trailing line
// ending, because a secret MAY legitimately contain whitespace. That
// contract is right for a generic getter, so the trimming belongs here
// instead: PLEX_URL and PLEX_TOKEN are values where whitespace is
// meaningless, and passing it through would produce a malformed URL or
// a token Plex answers 401 to.
//
// Missing, unreadable, and whitespace-only values are all fatal. The
// blank check keeps that true across the trim — a padded KEY would
// otherwise trim down to an empty string and start the app with no URL
// or token at all.
func requireEnv(key envx.Key) string {
	v, err := envx.Secret(key)
	if err != nil {
		slog.Error("required environment variable is missing or unreadable", "key", string(key), "error", err)
		os.Exit(1)
	}
	v = strings.TrimSpace(v)
	if v == "" {
		slog.Error("required environment variable is blank", "key", string(key))
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

// loadSchedulerInterval parses DEEP_SCAN_INTERVAL and reports the daily
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
	raw := strings.TrimSpace(os.Getenv("DEEP_SCAN_INTERVAL"))
	if raw == "" {
		return interval, enabled
	}
	if lower := strings.ToLower(raw); lower == "off" || lower == "disabled" {
		return 0, false
	}
	d, err := time.ParseDuration(raw)
	switch {
	case err != nil:
		slog.Warn("cannot parse DEEP_SCAN_INTERVAL, using default",
			"value", raw, "default", defaultSchedulerInterval.String())
	case d == 0:
		// "0"/"0s" disables the daily safety-net pass.
		return 0, false
	case d < 0:
		// A negative duration is a likely typo, not a documented disable
		// sentinel (off/disabled/0/0s); warn and fall back to the default
		// rather than silently idling.
		slog.Warn("DEEP_SCAN_INTERVAL is negative, using default",
			"value", raw, "default", defaultSchedulerInterval.String())
	default:
		interval = d
	}
	return interval, enabled
}
