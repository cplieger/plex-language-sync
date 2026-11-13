// Package fakeapi provides shared concurrency-safe test fakes for the Plex
// client and the persisted cache. Consumes no I/O. Import from _test.go files
// only. The cache fake is one implementation shared by every package's tests,
// kept honest against the real cache by cachecontract.Run.
//
// The fakes implement the full surface of what they stand in for; each
// CONSUMER declares the narrow subset it uses, so a fake satisfying everything
// still cannot widen what production depends on.
package fakeapi

import (
	"cmp"
	"maps"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/cplieger/plex-language-sync/internal/streams"
)

// Cache is a concurrency-safe in-memory stand-in for the persisted cache.
// Every accessor takes a short-held lock; consumers can share one Cache
// across goroutines without additional synchronization.
//
// The surface is exactly cachecontract.Contract, and cachecontract.Run is run
// against it: consumers assert on the fake through the same readers production code
// uses (WasRecentlyProcessed, IntentFor, UserTokens) rather than through
// fake-only inspectors.
type Cache struct {
	processed    map[string]time.Time
	profiles     map[string]map[string]string
	intents      map[string]map[string]streams.Intent
	applied      map[string]map[string]streams.Applied
	tokens       map[string]string
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
		intents:      make(map[string]map[string]streams.Intent),
		applied:      make(map[string]map[string]streams.Applied),
		tokens:       make(map[string]string),
		recentWindow: 5 * time.Minute,
	}
}

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
// An empty choice.Audio is ignored to match internal/cache.Cache.
func (c *Cache) LearnLanguageProfile(userID string, choice streams.LanguageChoice) {
	audioLang, subtitleLang := choice.Audio, choice.Subtitle
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

// RecordIntent stores a user's observed track selection under a scope key,
// mirroring internal/cache.Cache: deep-copied, keyed on (user, key), nil
// intent and empty keys ignored.
func (c *Cache) RecordIntent(userID, key string, intent *streams.Intent) {
	if intent == nil || userID == "" || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.intents[userID] == nil {
		c.intents[userID] = make(map[string]streams.Intent)
	}
	c.intents[userID][key] = intent.Clone()
}

// IntentFor returns the intent recorded under exactly (user, key),
// deep-copied.
func (c *Cache) IntentFor(userID, key string) (streams.Intent, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	userIntents, ok := c.intents[userID]
	if !ok {
		return streams.Intent{}, false
	}
	intent, ok := userIntents[key]
	if !ok {
		return streams.Intent{}, false
	}
	return intent.Clone(), true
}

// NewestObservedAt returns the greatest ObservedAt among userID's intents
// whose key match accepts, or 0 when none does. match runs under the cache
// lock, so it must not read the cache.
func (c *Cache) NewestObservedAt(userID string, match func(key string) bool) int64 {
	c.mu.Lock()
	defer c.mu.Unlock()
	var newest int64
	byKey := c.intents[userID]
	for key := range byKey {
		if match(key) {
			newest = max(newest, byKey[key].ObservedAt)
		}
	}
	return newest
}

// ownedIntent is one recorded intent with the user and key it is stored under.
type ownedIntent struct {
	user, key string
	in        streams.Intent
}

// LatestIntentFor mirrors internal/cache.Cache: the newest accepted intent of
// another user, ties by user then key, deep-copied, match run unlocked.
func (c *Cache) LatestIntentFor(exceptUser string, match func(user, key string, in streams.Intent) bool) (in streams.Intent, key, fromUser string, ok bool) {
	c.mu.Lock()
	var all []ownedIntent
	for user, byKey := range c.intents {
		if user == exceptUser {
			continue
		}
		for k := range byKey {
			rec := byKey[k]
			all = append(all, ownedIntent{user: user, key: k, in: rec.Clone()})
		}
	}
	c.mu.Unlock()
	slices.SortFunc(all, func(a, b ownedIntent) int {
		return cmp.Or(cmp.Compare(b.in.ObservedAt, a.in.ObservedAt),
			strings.Compare(a.user, b.user), strings.Compare(a.key, b.key))
	})
	for i := range all {
		if o := &all[i]; match(o.user, o.key, o.in) {
			return o.in, o.key, o.user, true
		}
	}
	return streams.Intent{}, "", "", false
}

// AppliedFor returns the record of what the app left on one episode for one
// user, mirroring internal/cache.Cache.
func (c *Cache) AppliedFor(userID, ratingKey string) (streams.Applied, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.applied[userID][ratingKey]
	return a, ok
}

// RecordApplied stores the record for one episode and user, replacing any
// earlier one; empty keys are ignored, mirroring internal/cache.Cache.
func (c *Cache) RecordApplied(userID, ratingKey string, a streams.Applied) {
	if userID == "" || ratingKey == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.applied[userID] == nil {
		c.applied[userID] = make(map[string]streams.Applied)
	}
	c.applied[userID][ratingKey] = a
}

// ForgetApplied drops the record for one episode and user, if any.
func (c *Cache) ForgetApplied(userID, ratingKey string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	delete(c.applied[userID], ratingKey)
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
