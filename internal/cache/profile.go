package cache

import (
	"log/slog"

	"github.com/cplieger/langtag"
)

// profileKey canonicalizes a language code for use as a profile-map key, so
// that the several published spellings of one language stop fragmenting into
// separate entries: nor, nob, no and nb all key on "no". Applied on both the
// read and the write path, which is what lets an existing profiles.json keep
// resolving without a migration step.
//
// An unparseable code is returned unchanged rather than dropped, so a legacy
// entry written under a code this build cannot parse stays reachable.
func profileKey(lang string) string {
	if t, ok := langtag.Parse(lang); ok {
		return t.Language()
	}
	return lang
}

// LearnLanguageProfile records a user's audio→subtitle language preference.
// Empty audioLang is treated as "unknown" and ignored — this prevents the
// profile map from accumulating an empty-key entry for streams whose
// language is not reported by Plex.
func (c *Cache) LearnLanguageProfile(userID, audioLang, subtitleLang string) {
	if audioLang == "" {
		return
	}
	rawLang := audioLang
	audioLang = profileKey(audioLang)
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.LanguageProfiles == nil {
		c.data.LanguageProfiles = make(map[string]map[string]string)
	}
	if c.data.LanguageProfiles[userID] == nil {
		c.data.LanguageProfiles[userID] = make(map[string]string)
	}
	// Remove any entry left under a non-canonical spelling of the same
	// language. Without this a profiles.json written before canonicalization
	// keeps a stale value that the read path would find first, so every later
	// preference change would be silently discarded for an upgraded install.
	if rawLang != audioLang {
		delete(c.data.LanguageProfiles[userID], rawLang)
	}
	prev, exists := c.data.LanguageProfiles[userID][audioLang]
	if !exists || prev != subtitleLang {
		c.data.LanguageProfiles[userID][audioLang] = subtitleLang
		slog.Info("language profile updated",
			"user", userID,
			"audio_lang", audioLang,
			"subtitle_lang", subtitleLang)
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
	if lang, found := userProfiles[profileKey(audioLang)]; found {
		return lang, true
	}
	// Then the spelling as given, which is how an entry written by an older
	// version is still found before it has been relearned.
	lang, ok := userProfiles[audioLang]
	return lang, ok
}
