package cache

import (
	"fmt"
	"strings"
	"testing"

	"pgregory.net/rapid"
)

func TestCacheLanguageProfilePerUser(t *testing.T) {
	c := New()

	// User 1 prefers English subs for Japanese audio.
	c.LearnLanguageProfile("1", "jpn", "eng")
	// User 2 prefers no subs for Japanese audio.
	c.LearnLanguageProfile("2", "jpn", "")

	lang, ok := c.SubtitleLangForAudio("1", "jpn")
	if !ok || lang != "eng" {
		t.Errorf("user 1 jpn: got %q, %v; want eng, true", lang, ok)
	}

	lang, ok = c.SubtitleLangForAudio("2", "jpn")
	if !ok || lang != "" {
		t.Errorf("user 2 jpn: got %q, %v; want empty, true", lang, ok)
	}

	// Unknown user returns false.
	_, ok = c.SubtitleLangForAudio("999", "jpn")
	if ok {
		t.Error("expected false for unknown user")
	}
}

func TestCacheLearnLanguageProfileIgnoresEmptyAudio(t *testing.T) {
	c := New()
	c.LearnLanguageProfile("1", "", "eng")
	if len(c.data.LanguageProfiles) != 0 {
		t.Error("should not learn profile with empty audio lang")
	}
}

func TestCacheGetSubtitleLangForAudioNilProfiles(t *testing.T) {
	var c Cache
	// Don't initialize LanguageProfiles — test nil map path.
	lang, ok := c.SubtitleLangForAudio("1", "eng")
	if ok || lang != "" {
		t.Errorf("expected empty/false for nil profiles, got %q, %v", lang, ok)
	}
}

func TestCacheLearnLanguageProfileIdempotent(t *testing.T) {
	c := New()

	c.LearnLanguageProfile("1", "jpn", "eng")
	c.LearnLanguageProfile("1", "jpn", "eng") // same value — should not log again

	lang, ok := c.SubtitleLangForAudio("1", "jpn")
	if !ok || lang != "eng" {
		t.Errorf("SubtitleLangForAudio(1, jpn) = (%q, %v), want (%q, true)", lang, ok, "eng")
	}
}

func TestCacheLearnLanguageProfileUpdate(t *testing.T) {
	c := New()

	c.LearnLanguageProfile("1", "jpn", "eng")
	if got, ok := c.SubtitleLangForAudio("1", "jpn"); !ok || got != "eng" {
		t.Fatalf("SubtitleLangForAudio(1, jpn) = (%q, %v), want (%q, true)", got, ok, "eng")
	}

	c.LearnLanguageProfile("1", "jpn", "fre")
	if got, ok := c.SubtitleLangForAudio("1", "jpn"); !ok || got != "fre" {
		t.Errorf("SubtitleLangForAudio(1, jpn) after update = (%q, %v), want (%q, true)", got, ok, "fre")
	}
}

func TestCacheLearnLanguageProfileMultipleLanguages(t *testing.T) {
	c := New()

	c.LearnLanguageProfile("1", "jpn", "eng")
	c.LearnLanguageProfile("1", "kor", "eng")
	c.LearnLanguageProfile("1", "eng", "")

	if lang, ok := c.SubtitleLangForAudio("1", "jpn"); !ok || lang != "eng" {
		t.Errorf("jpn profile: got %q, %v", lang, ok)
	}
	if lang, ok := c.SubtitleLangForAudio("1", "kor"); !ok || lang != "eng" {
		t.Errorf("kor profile: got %q, %v", lang, ok)
	}
	if lang, ok := c.SubtitleLangForAudio("1", "eng"); !ok || lang != "" {
		t.Errorf("eng profile: got %q, %v (want empty string, true)", lang, ok)
	}
	if _, ok := c.SubtitleLangForAudio("1", "fre"); ok {
		t.Error("fre profile should not exist")
	}
}

func TestCacheLearnLanguageProfileNilMaps(t *testing.T) {
	t.Parallel()
	var c Cache
	// Don't initialize LanguageProfiles — test nil map initialization path.
	c.LearnLanguageProfile("1", "jpn", "eng")

	lang, ok := c.SubtitleLangForAudio("1", "jpn")
	if !ok {
		t.Fatal("expected profile to exist after learn")
	}
	if lang != "eng" {
		t.Errorf("SubtitleLangForAudio(1, jpn) = %q, want eng", lang)
	}
}

func TestCacheLearnLanguageProfileNoChange(t *testing.T) {
	t.Parallel()
	c := New()
	c.data.LanguageProfiles = map[string]map[string]string{
		"1": {"jpn": "eng"},
	}
	// Call with the same value. The stored value must not change. The KEY does
	// move to its canonical spelling on the first write after an upgrade, which
	// is what stops a legacy entry shadowing later updates, so this reads through
	// the accessor rather than reaching for a raw map key.
	c.LearnLanguageProfile("1", "jpn", "eng")

	if got, ok := c.SubtitleLangForAudio("1", "jpn"); !ok || got != "eng" {
		t.Errorf("SubtitleLangForAudio(1, jpn) = (%q, %v), want (%q, true)", got, ok, "eng")
	}

	// Calling again with the same value is a genuine no-op: the key has already
	// been canonicalized, so nothing is written and nothing is logged.
	c.LearnLanguageProfile("1", "jpn", "eng")
	if got, ok := c.SubtitleLangForAudio("1", "jpn"); !ok || got != "eng" {
		t.Errorf("SubtitleLangForAudio(1, jpn) after a repeat call = (%q, %v), want (%q, true)", got, ok, "eng")
	}
}

// --- PBT: LearnLanguageProfile last-write-wins + empty-audio no-op ---

func TestLearnLanguageProfile_LastWriteWinsPBT(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := New()

		nWrites := rapid.IntRange(1, 20).Draw(t, "n_writes")
		expect := make(map[string]string)
		for i := range nWrites {
			user := rapid.SampledFrom([]string{"1", "2", "3"}).Draw(t, fmt.Sprintf("u_%d", i))
			audio := rapid.SampledFrom([]string{"eng", "jpn", "kor", "fra"}).Draw(t, fmt.Sprintf("a_%d", i))
			sub := rapid.SampledFrom([]string{"", "eng", "jpn", "kor", "fra"}).Draw(t, fmt.Sprintf("s_%d", i))
			c.LearnLanguageProfile(user, audio, sub)
			expect[user+"|"+audio] = sub
		}
		for k, want := range expect {
			parts := strings.SplitN(k, "|", 2)
			user, audio := parts[0], parts[1]
			got, ok := c.SubtitleLangForAudio(user, audio)
			if !ok {
				t.Errorf("SubtitleLangForAudio(%q,%q): not found, want %q", user, audio, want)
				continue
			}
			if got != want {
				t.Errorf("SubtitleLangForAudio(%q,%q) = %q, want %q (last-write-wins)", user, audio, got, want)
			}
		}
	})
}

func TestLearnLanguageProfile_EmptyAudioIsNoOpPBT(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		c := New()

		user := rapid.SampledFrom([]string{"1", "2"}).Draw(t, "user")
		sub := rapid.String().Draw(t, "sub")
		c.LearnLanguageProfile(user, "", sub)

		if profiles, ok := c.data.LanguageProfiles[user]; ok {
			if _, hasEmpty := profiles[""]; hasEmpty {
				t.Errorf("LearnLanguageProfile with empty audio created a %q entry", "")
			}
		}
	})
}

// TestCacheLanguageProfileDoesNotFragmentAcrossSpellings pins the second half of
// the reported bug. Plex reports Norwegian Bokmål as nob on one episode and nor
// on another, so a profile learned from one used to be invisible to a lookup
// made with the other, and the app would seed a new show with no subtitle at
// all. Both now key on the canonical language.
func TestCacheLanguageProfileDoesNotFragmentAcrossSpellings(t *testing.T) {
	c := New()
	c.LearnLanguageProfile("1", "nob", "nob")

	for _, spelling := range []string{"nob", "nor", "nb", "no"} {
		got, ok := c.SubtitleLangForAudio("1", spelling)
		if !ok {
			t.Errorf("SubtitleLangForAudio(1, %q) ok = false, want true; a profile learned as nob must answer for every spelling of Norwegian", spelling)
			continue
		}
		if got != "nob" {
			t.Errorf("SubtitleLangForAudio(1, %q) = %q, want %q", spelling, got, "nob")
		}
	}
}

// TestCacheLanguageProfileLegacyKeyStillResolves covers an existing
// profiles.json written before canonicalization: the raw key must keep
// answering, so the change needs no migration step.
func TestCacheLanguageProfileLegacyKeyStillResolves(t *testing.T) {
	c := New()
	c.mu.Lock()
	c.data.LanguageProfiles = map[string]map[string]string{"1": {"jpn": "eng"}}
	c.mu.Unlock()

	// The app always looks up with the code Plex reported, which is the same
	// spelling the legacy entry was written under, so the raw key answers.
	if got, ok := c.SubtitleLangForAudio("1", "jpn"); !ok || got != "eng" {
		t.Errorf("SubtitleLangForAudio(1, jpn) with a legacy raw key = (%q, %v), want (%q, true)", got, ok, "eng")
	}

	// A legacy entry is NOT reachable by a different spelling of the same
	// language until it is relearned, because folding the stored keys on load
	// would have to resolve collisions between two entries naming one language
	// and there is no non-arbitrary way to pick. Recorded here so the boundary
	// is a decision rather than a surprise: the next play rewrites the entry
	// under the canonical key and every spelling resolves from then on.
	if _, ok := c.SubtitleLangForAudio("1", "ja"); ok {
		t.Error("SubtitleLangForAudio(1, ja) against a legacy jpn key resolved; the documented boundary is that it does not until relearned")
	}

	c.LearnLanguageProfile("1", "jpn", "eng")
	for _, spelling := range []string{"jpn", "ja"} {
		if got, ok := c.SubtitleLangForAudio("1", spelling); !ok || got != "eng" {
			t.Errorf("SubtitleLangForAudio(1, %q) after relearning = (%q, %v), want (%q, true)", spelling, got, ok, "eng")
		}
	}
}

// TestCacheLanguageProfileLegacyKeyDoesNotShadowUpdates pins the second defect
// three reviewers found in the diff.
//
// The write path records the canonical key while the read path used to check the
// spelling as given first, so an entry in a profiles.json written by an older
// version answered every lookup forever and every later preference change was
// silently discarded. In the collision shape it could answer with the empty
// string, which the caller reads as "the user wants no subtitles" and acts on by
// disabling them.
func TestCacheLanguageProfileLegacyKeyDoesNotShadowUpdates(t *testing.T) {
	c := New()
	c.mu.Lock()
	c.data.LanguageProfiles = map[string]map[string]string{"1": {"jpn": "eng"}}
	c.mu.Unlock()

	// The user watches something and changes their preference.
	c.LearnLanguageProfile("1", "jpn", "fre")

	for _, spelling := range []string{"jpn", "ja"} {
		got, ok := c.SubtitleLangForAudio("1", spelling)
		if !ok {
			t.Errorf("SubtitleLangForAudio(1, %q) ok = false, want true", spelling)
			continue
		}
		if got != "fre" {
			t.Errorf("SubtitleLangForAudio(1, %q) = %q, want %q; a legacy key must not shadow a newer value",
				spelling, got, "fre")
		}
	}

	// The stale spelling must be gone rather than merely outranked, so it cannot
	// resurface if the read order ever changes again.
	c.mu.Lock()
	_, stale := c.data.LanguageProfiles["1"]["jpn"]
	c.mu.Unlock()
	if stale {
		t.Error("the non-canonical key jpn is still present after a relearn, want it removed")
	}
}

// TestCacheLanguageProfileEmptyValueIsNotShadowed covers the damaging shape of
// the same defect: a learned "no subtitles for this audio" preference is a
// legitimate empty string, so a stale non-empty legacy value must not answer in
// its place, and vice versa.
func TestCacheLanguageProfileEmptyValueIsNotShadowed(t *testing.T) {
	c := New()
	c.mu.Lock()
	c.data.LanguageProfiles = map[string]map[string]string{"1": {"jpn": "eng"}}
	c.mu.Unlock()

	// The user turns subtitles off for Japanese audio.
	c.LearnLanguageProfile("1", "jpn", "")

	got, ok := c.SubtitleLangForAudio("1", "jpn")
	if !ok {
		t.Fatal("SubtitleLangForAudio(1, jpn) ok = false, want true")
	}
	if got != "" {
		t.Errorf("SubtitleLangForAudio(1, jpn) = %q, want the empty string; the user chose no subtitles and a stale value answered instead",
			got)
	}
}

// TestCacheLanguageProfileReadPrefersCanonicalKey pins the READ order, which the
// sibling tests do not reach.
//
// The write path deletes the exact spelling it was handed, so a relearn arriving
// under the same spelling as a legacy entry passes even if reads check the raw
// spelling first. Plex reports the same language under more than one code, so the
// relearn can arrive as "nor" while the legacy entry sits under "nob". Only a
// canonical-first read answers correctly then, and without this test that order
// could be reverted with the whole suite still green.
func TestCacheLanguageProfileReadPrefersCanonicalKey(t *testing.T) {
	c := New()
	c.mu.Lock()
	c.data.LanguageProfiles = map[string]map[string]string{"1": {"nob": "eng"}}
	c.mu.Unlock()

	// The relearn arrives under a different spelling of the same language, so it
	// is written under the canonical key and cannot delete the legacy one.
	c.LearnLanguageProfile("1", "nor", "fre")

	for _, spelling := range []string{"nob", "nor", "nb", "no"} {
		got, ok := c.SubtitleLangForAudio("1", spelling)
		if !ok {
			t.Errorf("SubtitleLangForAudio(1, %q) ok = false, want true", spelling)
			continue
		}
		if got != "fre" {
			t.Errorf("SubtitleLangForAudio(1, %q) = %q, want %q; the canonical key must be read before the spelling as given",
				spelling, got, "fre")
		}
	}
}
