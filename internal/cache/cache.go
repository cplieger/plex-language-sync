// Package cache is the on-disk persistence layer for processed-episode
// deduplication, per-user language profiles, shared-user tokens, and the
// scheduler's last-run marker.
//
// The persisted JSON schema (field names, types, tags) is an inviolate
// contract — the on-disk /config/cache.json file is read-forward /
// write-back across deploys, so any schema change is a migration, not a
// refactor.
package cache

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"sync"

	"github.com/cplieger/atomicfile/v2"
	"github.com/cplieger/plex-language-sync/internal/api"
)

// Compile-time interface satisfaction assertion.
var _ api.Cache = (*Cache)(nil)

// maxCacheSize caps the cache file at 50 MB. A file at this size is almost
// certainly corrupted or deliberately bloated; we warn and proceed rather
// than refusing to start.
const maxCacheSize = 50 << 20 // 50 MB

// Data is the JSON schema persisted to /config/cache.json. Field names and
// JSON tags are frozen — the on-disk file is read-forward across deploys.
type Data struct {
	// ProcessedEpisodes tracks recently processed episode keys to avoid
	// re-processing the same episode on rapid successive events.
	// Keys include userID: "play:{userID}:{ratingKey}".
	ProcessedEpisodes map[string]int64 `json:"processed_episodes"`
	// LanguageProfiles maps userID → audioLang → subtitleLang.
	// Empty subtitle string means "no subtitles" for that audio language.
	LanguageProfiles map[string]map[string]string `json:"language_profiles"`
	// UserTokens maps userID → accessToken for shared users.
	UserTokens map[string]string `json:"user_tokens"`
	// LastSchedulerRun is the unix timestamp of the last scheduler run.
	LastSchedulerRun int64 `json:"last_scheduler_run"`
}

// Cache is the concurrent-safe persistent cache. The zero value is usable;
// prefer New for explicit initialization of the backing maps.
type Cache struct {
	data Data
	mu   sync.Mutex
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
			UserTokens:        make(map[string]string),
		},
	}
}

// LoadFrom reads the cache from the given path. A missing file returns nil
// (fresh start). Capped at maxCacheSize bytes via atomicfile.ReadBounded. Warns
// if the file has permissive mode bits set, as tokens are stored here.
func (c *Cache) LoadFrom(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data.ProcessedEpisodes = make(map[string]int64)
	c.data.LanguageProfiles = make(map[string]map[string]string)
	c.data.UserTokens = make(map[string]string)

	info, statErr := os.Stat(path)
	if statErr != nil {
		if errors.Is(statErr, os.ErrNotExist) {
			return nil // missing file = fresh start
		}
		return statErr
	}
	if info.Mode().Perm()&0o077 != 0 {
		slog.Warn("cache file has permissive mode; user tokens may be "+
			"readable by other host users",
			"path", path, "mode", info.Mode().Perm().String())
	}
	// ReadBounded caps the read at maxCacheSize; an oversize file returns an
	// error (caller starts fresh) rather than truncating. Unmarshal then maps
	// the bytes onto the frozen schema.
	raw, err := atomicfile.ReadBounded(context.Background(), path, maxCacheSize)
	if err != nil {
		return err
	}
	return json.Unmarshal(raw, &c.data)
}

// SaveTo atomically writes the cache to the given path (temp file + rename)
// and ensures the final file is 0o600 (user tokens live here). The temp
// file is removed on any failure path so partial writes don't clutter the
// dir.
func (c *Cache) SaveTo(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.pruneOldEntriesLocked()

	data, err := json.MarshalIndent(&c.data, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal: %w", err)
	}

	// The token cache is user-only (0o600). SaveBytes used to auto-create the
	// parent dir at a perm derived from the file perm (0o700 when the file perm
	// has no group/other bits, else 0o755); preserve that via WithMkdirMode.
	// Our file perm is 0o600 (0o600&0o077 == 0), so the derived dir perm is 0o700.
	res, err := atomicfile.WriteFile(context.Background(), path, data,
		atomicfile.WithMode(0o600), atomicfile.WithMkdirMode(0o700))
	if err != nil {
		// A non-nil error is now unambiguously a real failure: the data did
		// not reach its final path.
		return err
	}
	if !res.Durable {
		// cache.json is reconstructible: a non-durable result means the cache
		// reached disk but the parent-dir fsync was unconfirmed, so durability
		// across an immediate crash is not guaranteed. The data is present and
		// would be rebuilt from Plex on the next run anyway, so warn rather
		// than fail.
		slog.Warn("cache written but parent-dir fsync unconfirmed; not guaranteed durable across an immediate crash",
			"path", path)
	}
	slog.Debug("cache saved", "path", path, "bytes", len(data))
	return nil
}
