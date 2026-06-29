package api

import "time"

// Cache is the persistent cache consumed by the sync / scheduler / user-
// management subsystems. The concrete implementation lives in
// internal/cache.
//
// LoadFrom / SaveTo are deliberately NOT on this interface — they are
// composition-root concerns (wiring the cache to a path on disk) and do
// not belong on the abstraction consumers see.
type Cache interface {
	WasRecentlyProcessed(key string) bool
	MarkProcessed(key string)
	CheckAndMark(key string) bool
	LearnLanguageProfile(userID, audioLang, subtitleLang string)
	SubtitleLangForAudio(userID, audioLang string) (string, bool)
	UserTokens() map[string]string
	SetUserTokens(tokens map[string]string)
	LastSchedulerRun() time.Time
	SetLastSchedulerRun(t time.Time)
}
