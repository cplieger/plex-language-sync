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
	"io"
	"log/slog"
	"os"
	"sync"

	"github.com/cplieger/atomicfile"

	"plex-language-sync/internal/api"
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
// (fresh start). Capped at maxCacheSize bytes via io.LimitReader. Warns if
// the file has permissive mode bits set, as tokens are stored here.
func (c *Cache) LoadFrom(path string) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.data.ProcessedEpisodes = make(map[string]int64)
	c.data.LanguageProfiles = make(map[string]map[string]string)
	c.data.UserTokens = make(map[string]string)

	f, err := os.Open(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		return err
	}
	defer f.Close()

	if info, statErr := f.Stat(); statErr == nil {
		if info.Mode().Perm()&0o077 != 0 {
			slog.Warn("cache file has permissive mode; user tokens may be "+
				"readable by other host users",
				"path", path,
				"mode", info.Mode().Perm().String())
		}
	}

	data, err := io.ReadAll(io.LimitReader(f, maxCacheSize))
	if err != nil {
		return err
	}
	if int64(len(data)) >= maxCacheSize {
		slog.Warn("cache file at size limit, may be truncated",
			"path", path, "bytes", len(data), "limit", maxCacheSize)
	}
	return json.Unmarshal(data, &c.data)
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

	if err := atomicfile.WriteFile(context.Background(), path, data, atomicfile.WithMode(0o600)); err != nil {
		return err
	}
	slog.Debug("cache saved", "path", path, "bytes", len(data))
	return nil
}
