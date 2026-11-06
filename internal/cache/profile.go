package cache

import (
	"log/slog"

	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/plex-language-sync/internal/streams"
)

// LearnLanguageProfile records a user's audio→subtitle language preference.
// An empty choice.Audio is treated as "unknown" and ignored — this prevents
// the profile map from accumulating an empty-key entry for streams whose
// language is not reported by Plex.
func (c *Cache) LearnLanguageProfile(userID string, choice streams.LanguageChoice) {
	audioLang, subtitleLang := choice.Audio, choice.Subtitle
	if audioLang == "" {
		return
	}
	audioLang = streams.ProfileKey(audioLang)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.LanguageProfiles == nil {
		c.data.LanguageProfiles = make(map[string]map[string]string)
	}
	if c.data.LanguageProfiles[userID] == nil {
		c.data.LanguageProfiles[userID] = make(map[string]string)
	}
	// Remove every entry left under a non-canonical spelling of this language,
	// not only the spelling handed in. Without this a profiles.json written
	// before canonicalization keeps a stale value, and Plex reports the same
	// language under more than one code, so the surviving spelling need not be
	// the one that arrives next. Sweeping the user's own entries is cheap: the
	// map holds one entry per language the user has watched.
	for key := range c.data.LanguageProfiles[userID] {
		if key != audioLang && streams.ProfileKey(key) == audioLang {
			delete(c.data.LanguageProfiles[userID], key)
		}
	}
	prev, exists := c.data.LanguageProfiles[userID][audioLang]
	if !exists || prev != subtitleLang {
		c.data.LanguageProfiles[userID][audioLang] = subtitleLang
		slog.Info("language profile updated",
			"user", textsafe.SanitizeSingleLine(userID),
			"audio_lang", textsafe.SanitizeSingleLine(audioLang),
			"subtitle_lang", textsafe.SanitizeSingleLine(subtitleLang))
	}
}

// SubtitleLangForAudio returns the learned subtitle language for a given
// audio language and user. Returns ("", false) if no profile exists.
func (c *Cache) SubtitleLangForAudio(userID, audioLang string) (string, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.LanguageProfiles == nil {
		return "", false
	}
	userProfiles, ok := c.data.LanguageProfiles[userID]
	if !ok {
		return "", false
	}
	// The canonical key is authoritative, because that is what the write path
	// records. Checking the raw spelling first would let a legacy entry shadow
	// every value written after the upgrade.
	if lang, found := userProfiles[streams.ProfileKey(audioLang)]; found {
		return lang, true
	}
	// Then the spelling as given, which is how an entry written by an older
	// version is still found before it has been relearned.
	lang, ok := userProfiles[audioLang]
	return lang, ok
}
