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

	lang := c.data.LanguageProfiles["1"]["jpn"]
	if lang != "eng" {
		t.Errorf("expected eng, got %q", lang)
	}
}

func TestCacheLearnLanguageProfileUpdate(t *testing.T) {
	c := New()

	c.LearnLanguageProfile("1", "jpn", "eng")
	if c.data.LanguageProfiles["1"]["jpn"] != "eng" {
		t.Fatal("initial profile not set")
	}

	c.LearnLanguageProfile("1", "jpn", "fre")
	if c.data.LanguageProfiles["1"]["jpn"] != "fre" {
		t.Errorf("profile should update to fre, got %q", c.data.LanguageProfiles["1"]["jpn"])
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
	// Call with same value — should be a no-op (no log, no change).
	c.LearnLanguageProfile("1", "jpn", "eng")

	lang := c.data.LanguageProfiles["1"]["jpn"]
	if lang != "eng" {
		t.Errorf("LanguageProfiles[1][jpn] = %q, want eng", lang)
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
