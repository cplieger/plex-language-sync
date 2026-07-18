// Package cache is the on-disk persistence layer for processed-episode
// deduplication, per-user language profiles, shared-user tokens, and the
// scheduler's last-run marker.
//
// On-disk layout: state is split across three files in the cache dir by
// retention class, so one corrupt file never costs another class's state:
//
//	profiles.json  language_profiles                     irreplaceable learned state
//	tokens.json    user_tokens                           re-fetchable encrypted secrets
//	state.json     processed_episodes, last_scheduler_run disposable operational state
//
// Field names, types, and JSON tags within each file are an inviolate
// read-forward / write-back contract across deploys — any change is a
// migration, not a refactor. A pre-split /config/cache.json (the legacy
// union schema, see Data) is migrated automatically on first load: its
// sections seed anything a split file does not yet cover, the split files
// are then written eagerly, and the legacy file is removed once all three
// split files exist on disk. Per-section precedence is
// split-file > legacy > fresh, so a partially failed migration can never
// lose a section that either source still holds.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
	"sync"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/plex-language-sync/internal/api"
	"github.com/cplieger/plex-language-sync/internal/streams"
)

// Compile-time interface satisfaction assertion.
var _ api.Cache = (*Cache)(nil)

// maxCacheSize caps each cache file at 50 MB. A file at this size is almost
// certainly corrupted or deliberately bloated; the loader warns and leaves
// that section in its reset state rather than truncating the read.
const maxCacheSize = 50 << 20 // 50 MB

// Split-layout file names. The names are part of the on-disk schema.
const (
	profilesFile = "profiles.json"
	tokensFile   = "tokens.json"
	stateFile    = "state.json"
	// legacyCacheFile is the pre-split union file, read only for migration.
	legacyCacheFile = "cache.json"
)

// Data is the in-memory state shape. It doubles as the decode target for
// the legacy pre-split cache.json union schema, whose field names and JSON
// tags it preserves verbatim (read-forward contract).
type Data struct {
	// ProcessedEpisodes tracks recently processed episode keys to avoid
	// re-processing the same episode on rapid successive events.
	// Keys carry a subsystem prefix from keys.go:
	// "streams:{userID}:{ratingKey}:{audioID}:{subID}" (play events),
	// "timeline:{itemID}" (library scans), and "scheduler:{ratingKey}"
	// (deep-analysis runs). Persisted in state.json.
	ProcessedEpisodes map[string]int64 `json:"processed_episodes"`
	// LanguageProfiles maps userID → audioLang → subtitleLang.
	// Empty subtitle string means "no subtitles" for that audio language.
	// Persisted in profiles.json.
	LanguageProfiles map[string]map[string]string `json:"language_profiles"`
	// Intents maps userID → showRatingKey → the user's last observed
	// track selection for that show (see streams.Intent). Persisted in
	// profiles.json; absent from the legacy union schema (pre-intent
	// versions), so legacy migration leaves it empty.
	Intents map[string]map[string]streams.Intent `json:"intents,omitempty"`
	// UserTokens maps userID → accessToken for shared users. Persisted in
	// tokens.json, encrypted at the disk boundary when a key is set.
	UserTokens map[string]string `json:"user_tokens"`
	// LastSchedulerRun is the unix timestamp of the last scheduler run.
	// Persisted in state.json.
	LastSchedulerRun int64 `json:"last_scheduler_run"`
}

// profilesData is the profiles.json schema (irreplaceable learned state).
type profilesData struct {
	LanguageProfiles map[string]map[string]string         `json:"language_profiles"`
	Intents          map[string]map[string]streams.Intent `json:"intents,omitempty"`
}

// tokensData is the tokens.json schema (re-fetchable encrypted secrets).
type tokensData struct {
	UserTokens map[string]string `json:"user_tokens"`
}

// stateData is the state.json schema (disposable operational state).
type stateData struct {
	ProcessedEpisodes map[string]int64 `json:"processed_episodes"`
	LastSchedulerRun  int64            `json:"last_scheduler_run"`
}

// Cache is the concurrent-safe persistent cache. The zero value is usable;
// prefer New for explicit initialization of the backing maps.
type Cache struct {
	data   Data
	encKey []byte // AES-256 key for user-token encryption at rest; nil = no encryption
	mu     sync.Mutex
}

// New returns a Cache with its maps pre-initialized. The zero value also
// works (maps are lazily created by the mutation methods) — New is the
// preferred construction point for application code because it documents
// intent.
func New() *Cache {
	return &Cache{
		data: Data{
			ProcessedEpisodes: make(map[string]int64),
			LanguageProfiles:  make(map[string]map[string]string),
			Intents:           make(map[string]map[string]streams.Intent),
			UserTokens:        make(map[string]string),
		},
	}
}

// SetEncryptionKey configures the AES-256 key used to encrypt user tokens
// at the disk boundary (Save/Load). The key should be derived from
// the admin PLEX_TOKEN via DeriveKey. When nil, tokens are stored and read
// as plaintext (backward-compatible with pre-encryption cache files).
func (c *Cache) SetEncryptionKey(key []byte) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.encKey = key
}

// Load reads the cache from the split-layout files in dir, migrating a
// legacy cache.json when present. Missing files are a fresh start for
// their section; a corrupt or oversized file resets ONLY its own section
// (the others load normally). The returned error joins every per-section
// failure — a non-nil return means at least one section started fresh,
// not that the whole cache did.
func (c *Cache) Load(dir string) error {
	migrate, err := c.loadLocked(dir)

	legacyPath := filepath.Join(dir, legacyCacheFile)
	switch {
	case migrate:
		// Eagerly write the split files so the migration completes in one
		// load; remove the legacy file only once all three exist on disk.
		if saveErr := c.Save(dir); saveErr != nil {
			slog.Warn("cache: migration save failed; legacy cache.json retained, will retry on next save",
				"dir", dir, "error", saveErr)
			return err
		}
		if rmErr := os.Remove(legacyPath); rmErr != nil {
			slog.Warn("cache: could not remove legacy cache.json after migration",
				"path", legacyPath, "error", rmErr)
		} else {
			slog.Info("cache migrated to split layout",
				"dir", dir, "files", []string{profilesFile, tokensFile, stateFile})
		}
	case err == nil && fileExists(legacyPath) && fileExists(filepath.Join(dir, profilesFile)) &&
		fileExists(filepath.Join(dir, tokensFile)) && fileExists(filepath.Join(dir, stateFile)):
		// Split layout is complete and authoritative; the legacy file is a
		// stale leftover (e.g. an interrupted earlier migration cleanup).
		if rmErr := os.Remove(legacyPath); rmErr != nil {
			slog.Warn("cache: could not remove stale legacy cache.json",
				"path", legacyPath, "error", rmErr)
		} else {
			slog.Info("cache: removed stale legacy cache.json (split layout is authoritative)",
				"path", legacyPath)
		}
	}
	return err
}

// loadLocked resets in-memory state and loads it back from disk under the
// lock: legacy baseline first (when present), then each split file
// overlaying its own section. It returns whether an eager migration save
// is owed (legacy decoded, split layout incomplete) and the joined
// per-section error.
func (c *Cache) loadLocked(dir string) (migrate bool, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data = Data{
		ProcessedEpisodes: make(map[string]int64),
		LanguageProfiles:  make(map[string]map[string]string),
		Intents:           make(map[string]map[string]streams.Intent),
		UserTokens:        make(map[string]string),
	}

	var errs []error

	// Legacy baseline: sections whose split file is missing inherit from it.
	legacyFound, legacyOK := false, false
	var legacy Data
	if found, lerr := loadJSONFile(filepath.Join(dir, legacyCacheFile), true, &legacy); found {
		legacyFound = true
		if lerr != nil {
			slog.Warn("cache: legacy cache.json unreadable or corrupt; not used as migration baseline",
				"dir", dir, "error", lerr)
			errs = append(errs, lerr)
		} else {
			legacyOK = true
			c.applyLegacyLocked(&legacy)
		}
	} else if lerr != nil {
		errs = append(errs, lerr)
	}

	// Split files overlay the baseline per section.
	splitPresent := 0
	splitPresent += c.overlayProfilesLocked(dir, &errs)
	splitPresent += c.overlayTokensLocked(dir, &errs)
	splitPresent += c.overlayStateLocked(dir, &errs)

	if !legacyFound && splitPresent == 0 && len(errs) == 0 {
		slog.Info("cache files not found, starting fresh", "dir", dir)
		return false, nil
	}

	slog.Debug("cache loaded",
		"dir", dir,
		"processed_episodes", len(c.data.ProcessedEpisodes),
		"language_profiles", len(c.data.LanguageProfiles),
		"user_tokens", len(c.data.UserTokens))
	return legacyOK && splitPresent < 3, errors.Join(errs...)
}

// applyLegacyLocked seeds in-memory state from a decoded legacy union
// file, decrypting tokens and normalizing nil maps. Caller holds c.mu.
func (c *Cache) applyLegacyLocked(legacy *Data) {
	if legacy.ProcessedEpisodes != nil {
		c.data.ProcessedEpisodes = legacy.ProcessedEpisodes
	}
	if legacy.LanguageProfiles != nil {
		c.data.LanguageProfiles = legacy.LanguageProfiles
	}
	if legacy.Intents != nil {
		c.data.Intents = legacy.Intents
	}
	if legacy.UserTokens != nil {
		c.data.UserTokens = legacy.UserTokens
		c.decryptTokensLocked()
	}
	c.data.LastSchedulerRun = legacy.LastSchedulerRun
}

// overlayProfilesLocked loads profiles.json over the baseline. Returns 1
// when the file exists (regardless of decode success) so the caller can
// count split-layout presence. Caller holds c.mu.
func (c *Cache) overlayProfilesLocked(dir string, errs *[]error) int {
	var pd profilesData
	found, err := loadJSONFile(filepath.Join(dir, profilesFile), false, &pd)
	if !found {
		if err != nil {
			*errs = append(*errs, err)
		}
		return 0
	}
	if err != nil {
		slog.Warn("cache: profiles file unreadable or corrupt; learned profiles reset until re-learned from playback",
			"file", profilesFile, "error", err)
		*errs = append(*errs, err)
		return 1
	}
	c.data.LanguageProfiles = pd.LanguageProfiles
	if c.data.LanguageProfiles == nil {
		c.data.LanguageProfiles = make(map[string]map[string]string)
	}
	c.data.Intents = pd.Intents
	if c.data.Intents == nil {
		c.data.Intents = make(map[string]map[string]streams.Intent)
	}
	return 1
}

// overlayTokensLocked loads tokens.json over the baseline, decrypting
// values when a key is configured. Caller holds c.mu.
func (c *Cache) overlayTokensLocked(dir string, errs *[]error) int {
	var td tokensData
	found, err := loadJSONFile(filepath.Join(dir, tokensFile), true, &td)
	if !found {
		if err != nil {
			*errs = append(*errs, err)
		}
		return 0
	}
	if err != nil {
		slog.Warn("cache: tokens file unreadable or corrupt; cached tokens reset until refreshed from plex.tv",
			"file", tokensFile, "error", err)
		*errs = append(*errs, err)
		return 1
	}
	c.data.UserTokens = td.UserTokens
	if c.data.UserTokens == nil {
		c.data.UserTokens = make(map[string]string)
	}
	c.decryptTokensLocked()
	return 1
}

// overlayStateLocked loads state.json over the baseline. Caller holds c.mu.
func (c *Cache) overlayStateLocked(dir string, errs *[]error) int {
	var sd stateData
	found, err := loadJSONFile(filepath.Join(dir, stateFile), false, &sd)
	if !found {
		if err != nil {
			*errs = append(*errs, err)
		}
		return 0
	}
	if err != nil {
		slog.Warn("cache: state file unreadable or corrupt; dedup keys and scheduler marker reset",
			"file", stateFile, "error", err)
		*errs = append(*errs, err)
		return 1
	}
	c.data.ProcessedEpisodes = sd.ProcessedEpisodes
	if c.data.ProcessedEpisodes == nil {
		c.data.ProcessedEpisodes = make(map[string]int64)
	}
	c.data.LastSchedulerRun = sd.LastSchedulerRun
	return 1
}

// decryptTokensLocked decrypts every stored token value in place when an
// encryption key is configured. Plaintext values (pre-encryption files)
// pass through unchanged — on the next Save they are encrypted
// transparently. A value that fails to decrypt (e.g. after an admin-token
// rotation) is treated as stale and dropped; the next plex.tv refresh
// replaces it. Caller holds c.mu.
func (c *Cache) decryptTokensLocked() {
	if c.encKey == nil {
		return
	}
	for uid, val := range c.data.UserTokens {
		plain, decErr := DecryptToken(c.encKey, val)
		if decErr != nil {
			slog.Warn("cache: failed to decrypt user token, will refresh from plex.tv",
				"user", uid, "error", decErr)
			delete(c.data.UserTokens, uid)
			continue
		}
		c.data.UserTokens[uid] = plain
	}
}

// loadJSONFile stats, bounded-reads, and unmarshals one cache file.
// found=false means the file does not exist (fresh start for its section).
// warnPerms enables the permissive-mode warning for secret-bearing files
// (tokens.json and the legacy union file, which contains tokens).
func loadJSONFile(path string, warnPerms bool, into any) (found bool, err error) {
	info, statErr := os.Stat(path)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return false, nil
		}
		return false, statErr
	}
	if warnPerms && info.Mode().Perm()&0o077 != 0 {
		slog.Warn("cache file has permissive mode; user tokens may be "+
			"readable by other host users",
			"path", path, "mode", info.Mode().Perm().String())
	}
	// ReadBounded caps the read at maxCacheSize; an oversize file returns an
	// error (that section starts fresh) rather than truncating. Unmarshal
	// populates fields up to the error point, so decode into the caller's
	// zero-value target and let the caller commit only on success.
	raw, err := atomicfile.ReadBounded(context.Background(), path, maxCacheSize)
	if err != nil {
		return true, err
	}
	if err := json.Unmarshal(raw, into); err != nil {
		return true, err
	}
	return true, nil
}

// fileExists reports whether path exists (any stat success counts).
func fileExists(path string) bool {
	_, err := os.Stat(path)
	return err == nil
}

// Save atomically writes the three split-layout files into dir (each temp
// file + rename, 0o600 — tokens live in one of them and the uniform mode
// keeps the contract simple). Encoding for all three files happens first,
// under one lock acquisition, so the files are a consistent snapshot and
// an encode failure (e.g. token encryption) writes nothing at all. Disk
// writes then run lock-free so a concurrent MarkProcessed /
// WasRecentlyProcessed (the listener goroutine) never blocks on the
// scheduler goroutine's fsync. Write failures are per-file: the remaining
// files are still attempted and the joined error is returned.
func (c *Cache) Save(dir string) error {
	profiles, tokens, state, err := c.encodeAllForSave()
	if err != nil {
		return err
	}

	files := []struct {
		name string
		data []byte
	}{
		// Most precious first: a mid-save crash favors the irreplaceable
		// sections having landed.
		{profilesFile, profiles},
		{tokensFile, tokens},
		{stateFile, state},
	}
	var errs []error
	for _, f := range files {
		path := filepath.Join(dir, f.name)
		res, werr := atomicfile.WriteFile(context.Background(), path, f.data,
			atomicfile.WithMode(0o600), atomicfile.WithMkdirMode(0o700))
		if werr != nil {
			// A non-nil error is unambiguously a real failure: the data did
			// not reach its final path. Keep writing the other sections.
			errs = append(errs, fmt.Errorf("%s: %w", f.name, werr))
			continue
		}
		if !res.Durable {
			// The cache is reconstructible: a non-durable result means the
			// file reached disk but the parent-dir fsync was unconfirmed, so
			// durability across an immediate crash is not guaranteed. Warn
			// rather than fail.
			slog.Warn("cache file written but parent-dir fsync unconfirmed; not guaranteed durable across an immediate crash",
				"path", path)
		}
	}
	if len(errs) > 0 {
		return errors.Join(errs...)
	}
	slog.Debug("cache saved", "dir", dir,
		"profiles_bytes", len(profiles), "tokens_bytes", len(tokens), "state_bytes", len(state))
	return nil
}

// encodeAllForSave prunes stale entries, encrypts user tokens for the
// on-disk copy (without mutating in-memory state), and marshals the three
// split-layout payloads under a single lock acquisition so they form one
// consistent snapshot. Any encode error aborts the whole save — no file
// is written from a partially encoded snapshot.
func (c *Cache) encodeAllForSave() (profiles, tokens, state []byte, err error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneOldEntriesLocked()

	// Encrypt user tokens for the on-disk copy without mutating in-memory
	// state. A nil map stays nil so tokens.json keeps the frozen
	// "user_tokens": null form (schema contract).
	tokensOut := c.data.UserTokens
	if c.encKey != nil && len(c.data.UserTokens) > 0 {
		encrypted := make(map[string]string, len(c.data.UserTokens))
		for uid, plain := range c.data.UserTokens {
			ct, encErr := EncryptToken(c.encKey, plain)
			if encErr != nil {
				return nil, nil, nil, fmt.Errorf("encrypt token for user %s: %w", uid, encErr)
			}
			encrypted[uid] = ct
		}
		tokensOut = encrypted
	}

	if profiles, err = json.MarshalIndent(&profilesData{
		LanguageProfiles: c.data.LanguageProfiles,
		Intents:          c.data.Intents,
	}, "", "  "); err != nil {
		return nil, nil, nil, fmt.Errorf("marshal %s: %w", profilesFile, err)
	}
	if tokens, err = json.MarshalIndent(&tokensData{UserTokens: tokensOut}, "", "  "); err != nil {
		return nil, nil, nil, fmt.Errorf("marshal %s: %w", tokensFile, err)
	}
	if state, err = json.MarshalIndent(&stateData{
		ProcessedEpisodes: c.data.ProcessedEpisodes,
		LastSchedulerRun:  c.data.LastSchedulerRun,
	}, "", "  "); err != nil {
		return nil, nil, nil, fmt.Errorf("marshal %s: %w", stateFile, err)
	}
	return profiles, tokens, state, nil
}
