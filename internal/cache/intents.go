package cache

import (
	"log/slog"

	"github.com/cplieger/plex-language-sync/internal/streams"
)

// RecordIntent stores a user's observed track selection for a show,
// replacing any previous intent for the same (user, show) pair. The
// intent is deep-copied so the caller retains exclusive ownership of
// the passed value. A nil intent or empty userID/showKey is ignored —
// an intent that cannot be keyed is meaningless.
func (c *Cache) RecordIntent(userID, showKey string, intent *streams.Intent) {
	if intent == nil || userID == "" || showKey == "" {
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
	c.data.Intents[userID][showKey] = intent.Clone()
	slog.Debug("intent recorded",
		"user", userID,
		"show_key", showKey,
		"audio_lang", intent.Audio.LanguageCode)
}

// IntentFor returns the recorded intent for a (user, show) pair, deep-
// copied so callers cannot mutate cache state. ok=false when no intent
// has been observed for the pair.
func (c *Cache) IntentFor(userID, showKey string) (streams.Intent, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	userIntents, ok := c.data.Intents[userID]
	if !ok {
		return streams.Intent{}, false
	}
	intent, ok := userIntents[showKey]
	if !ok {
		return streams.Intent{}, false
	}
	return intent.Clone(), true
}
