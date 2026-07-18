package api

import (
	"time"

	"github.com/cplieger/plex-language-sync/internal/streams"
)

// Cache is the persistent cache consumed by the sync / scheduler / user-
// management subsystems. The concrete implementation lives in
// internal/cache.
//
// Load / Save are deliberately NOT on this interface — they are
// composition-root concerns (wiring the cache to a path on disk) and do
// not belong on the abstraction consumers see.
type Cache interface {
	WasRecentlyProcessed(key string) bool
	MarkProcessed(key string)
	CheckAndMark(key string) bool
	LearnLanguageProfile(userID, audioLang, subtitleLang string)
	SubtitleLangForAudio(userID, audioLang string) (string, bool)
	// RecordIntent stores a user's observed track selection for a show
	// (event-plane only: callers record what they witnessed at a resolved
	// play session, never a reconstructed attribution).
	RecordIntent(userID, showKey string, intent *streams.Intent)
	// IntentFor returns the recorded intent for a (user, show) pair.
	IntentFor(userID, showKey string) (streams.Intent, bool)
	UserTokens() map[string]string
	SetUserTokens(tokens map[string]string)
	LastSchedulerRun() time.Time
	SetLastSchedulerRun(t time.Time)
}
