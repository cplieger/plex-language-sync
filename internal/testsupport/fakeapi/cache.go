// Package fakeapi provides shared concurrency-safe test fakes for the
// interfaces in internal/api. Consumes no I/O. Import from _test.go
// files only — the package name carries no internal build tag, but
// callers across internal/{sync,scheduler,notify,users} tests
// previously declared three near-identical fakeCache types; this
// package consolidates them into one honest implementation.
package fakeapi

import (
	"maps"
	"sort"
	"sync"
	"time"

	"github.com/cplieger/plex-language-sync/internal/api"
)

// Cache is a concurrency-safe in-memory implementation of api.Cache.
// Every accessor takes a short-held lock; consumers can share one Cache
// across goroutines without additional synchronization.
//
// Beyond api.Cache, Cache exposes the Processed helper reader (NOT part
// of api.Cache) that tests use to inspect the fake's state after a run.
type Cache struct {
	processed    map[string]time.Time
	profiles     map[string]map[string]string
	tokens       map[string]string
	lastRun      time.Time
	recentWindow time.Duration
	mu           sync.Mutex
}

// NewCache returns a ready-to-use zero-seeded Cache with the default
// recent-window of 5 minutes, matching internal/cache.Cache's
// WasRecentlyProcessed behavior.
func NewCache() *Cache {
	return &Cache{
		processed:    make(map[string]time.Time),
		profiles:     make(map[string]map[string]string),
		tokens:       make(map[string]string),
		recentWindow: 5 * time.Minute,
	}
}

// Compile-time interface assertion: *Cache satisfies api.Cache so tests
// can assign it to an api.Cache-typed field.
var _ api.Cache = (*Cache)(nil)

// WasRecentlyProcessed reports whether the key was MarkProcessed'd
// within the recent window.
func (c *Cache) WasRecentlyProcessed(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ts, ok := c.processed[key]
	if !ok {
		return false
	}
	return time.Since(ts) < c.recentWindow
}

// MarkProcessed records the current time against the key.
func (c *Cache) MarkProcessed(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.processed[key] = time.Now()
}

// CheckAndMark atomically tests and sets the recent-processed window for key,
// mirroring internal/cache.Cache.CheckAndMark: it returns true and records the
// current time when key is outside the recent window, or false (leaving the
// existing timestamp intact) when it is inside. The check and the mark happen
// under a single lock acquisition so two concurrent callers cannot both observe
// "not processed" for the same key.
func (c *Cache) CheckAndMark(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if ts, ok := c.processed[key]; ok && time.Since(ts) < c.recentWindow {
		return false
	}
	c.processed[key] = time.Now()
	return true
}

// LearnLanguageProfile stores a user's audio→subtitle preference.
// Empty audioLang is ignored to match internal/cache.Cache.
func (c *Cache) LearnLanguageProfile(userID, audioLang, subtitleLang string) {
	if audioLang == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.profiles[userID] == nil {
		c.profiles[userID] = make(map[string]string)
	}
	c.profiles[userID][audioLang] = subtitleLang
}

// SubtitleLangForAudio returns the learned subtitle language for the
// given audio language and user.
func (c *Cache) SubtitleLangForAudio(userID, audioLang string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	userProfiles, ok := c.profiles[userID]
	if !ok {
		return "", false
	}
	lang, ok := userProfiles[audioLang]
	return lang, ok
}

// UserTokens returns a defensive copy of the userID → accessToken map.
func (c *Cache) UserTokens() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]string, len(c.tokens))
	maps.Copy(out, c.tokens)
	return out
}

// SetUserTokens replaces the token map wholesale. Nil clears the map
// to an empty non-nil value.
func (c *Cache) SetUserTokens(tokens map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := make(map[string]string, len(tokens))
	maps.Copy(next, tokens)
	c.tokens = next
}

// LastSchedulerRun returns the recorded last-run timestamp. Zero value
// indicates "never run".
func (c *Cache) LastSchedulerRun() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.lastRun
}

// SetLastSchedulerRun records the supplied timestamp.
func (c *Cache) SetLastSchedulerRun(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.lastRun = t
}

// ---------------------------------------------------------------------------
// Test helpers (NOT part of api.Cache)
// ---------------------------------------------------------------------------

// Processed returns a deterministically-ordered copy of the processed
// keys. Useful for asserting "the sync pass marked exactly these
// episodes as processed" without relying on Go's map-iteration order.
func (c *Cache) Processed() []string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make([]string, 0, len(c.processed))
	for k := range c.processed {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}
