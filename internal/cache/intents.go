package cache

import (
	"cmp"
	"log/slog"
	"slices"
	"strings"

	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/plex-language-sync/internal/streams"
)

// RecordIntent stores a user's observed track selection under key, a scope
// key string, replacing any previous intent for the same (user, key). The
// intent is deep-copied so the caller retains exclusive ownership of the
// passed value. A nil intent or an empty userID or key is ignored.
func (c *Cache) RecordIntent(userID, key string, intent *streams.Intent) {
	if intent == nil || userID == "" || key == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.Intents == nil {
		c.data.Intents = make(map[string]map[string]streams.Intent)
	}
	if c.data.Intents[userID] == nil {
		c.data.Intents[userID] = make(map[string]streams.Intent)
	}
	c.data.Intents[userID][key] = intent.Clone()
	slog.Debug("intent recorded",
		"user", textsafe.SanitizeSingleLine(userID),
		"show_key", textsafe.SanitizeSingleLine(key),
		"audio_lang", textsafe.SanitizeSingleLine(intent.Audio.LanguageCode))
}

// IntentFor returns the intent recorded under exactly (user, key), deep-
// copied so callers cannot mutate cache state. ok=false when none is.
func (c *Cache) IntentFor(userID, key string) (streams.Intent, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	userIntents, ok := c.data.Intents[userID]
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
	byKey := c.data.Intents[userID]
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

// LatestIntentFor returns the newest intent, by ObservedAt, that a user other
// than exceptUser recorded and match accepts, with its key and owner; ties go
// to the smaller user id, then the smaller key, in string order. The result is
// a deep copy. match runs with no cache lock held, so it may read this cache.
func (c *Cache) LatestIntentFor(exceptUser string, match func(user, key string, in streams.Intent) bool) (in streams.Intent, key, fromUser string, ok bool) {
	all := c.othersNewestFirst(exceptUser)
	for i := range all {
		if o := &all[i]; match(o.user, o.key, o.in) {
			return o.in, o.key, o.user, true
		}
	}
	return streams.Intent{}, "", "", false
}

// othersNewestFirst copies every intent not owned by exceptUser in
// LatestIntentFor's order. The copy is what lets the matcher run unlocked:
// mu is not reentrant, and a matcher reading the cache under it deadlocks.
func (c *Cache) othersNewestFirst(exceptUser string) []ownedIntent {
	c.mu.Lock()
	var out []ownedIntent
	for user, byKey := range c.data.Intents {
		if user == exceptUser {
			continue
		}
		for key := range byKey {
			in := byKey[key]
			out = append(out, ownedIntent{user: user, key: key, in: in.Clone()})
		}
	}
	c.mu.Unlock()
	slices.SortFunc(out, func(a, b ownedIntent) int {
		return cmp.Or(cmp.Compare(b.in.ObservedAt, a.in.ObservedAt),
			strings.Compare(a.user, b.user), strings.Compare(a.key, b.key))
	})
	return out
}
