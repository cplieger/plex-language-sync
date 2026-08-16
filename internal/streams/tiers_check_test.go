package streams

import (
	"testing"

	"github.com/cplieger/langtag"
)

// TestAudioAdmitsOnlyTheChosenLanguage is the whole audio contract in one table.
// Audio must give the language the viewer chose, or nothing. It may cross a
// region, and it may cross the script that gets inferred from a region, because
// a spoken track has no script and both sides of such a pair are still the same
// language. It may never cross to another language, however close.
func TestAudioAdmitsOnlyTheChosenLanguage(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		refTag, candTag string
		admit           bool
		why             string
	}{
		"identical":                {"nb", "nb", true, "same tag"},
		"macrolanguage spelling":   {"nb", "no", true, "nob and nor name one language"},
		"regional variant":         {"es-ES", "es-419", true, "one language, another region"},
		"inferred script artifact": {"zh-CN", "zh-TW", true, "one language; the script is inferred from the region"},
		"explicit script":          {"zh-Hans", "zh-Hant", true, "still one language"},
		"serbian scripts":          {"sr-Cyrl", "sr-Latn", true, "still one language"},

		"nynorsk for bokmal":     {"nb", "nn", false, "a different language, however close"},
		"danish for norwegian":   {"nb", "da", false, "a different language"},
		"swedish for norwegian":  {"nb", "sv", false, "unrelated"},
		"spanish for catalan":    {"ca", "es", false, "a different language"},
		"slovak for czech":       {"cs", "sk", false, "a different language"},
		"cantonese for mandarin": {"cmn", "yue", false, "a different language"},
		"english for anything":   {"ta", "en", false, "never, at any setting"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ref := &Stream{StreamType: StreamTypeAudio, LanguageTag: tc.refTag}
			cand := &Stream{ID: 7, StreamType: StreamTypeAudio, LanguageTag: tc.candTag}
			got := MatchAudio(ref, []*Stream{cand})
			if tc.admit && got == nil {
				t.Errorf("MatchAudio(%s, [%s]) = nil, want the track: %s", tc.refTag, tc.candTag, tc.why)
			}
			if !tc.admit && got != nil {
				t.Errorf("MatchAudio(%s, [%s]) = ID %d, want nil: %s", tc.refTag, tc.candTag, got.ID, tc.why)
			}
		})
	}
}

// TestSubtitleDefaultFloorAdmits is the same table for subtitles at the shipped
// default, which is one tier tighter: a script difference is real work for a
// reader even though it is not a different language.
func TestSubtitleDefaultFloorAdmits(t *testing.T) {
	t.Parallel()
	const defaultFloor = langtag.TierSameLanguage
	cases := map[string]struct {
		refTag, candTag string
		admit           bool
	}{
		"identical":              {"nb", "nb", true},
		"macrolanguage spelling": {"nb", "no", true},
		"regional variant":       {"es-ES", "es-419", true},
		"bare against regional":  {"pt", "pt-BR", true},

		// Serbian is written in both scripts and Serbian schooling teaches both,
		// so the pair reads as one language and the default floor accepts it.
		// The Unicode locale data agrees and is why this is not a judgment: it
		// scores the pair 5, against 50 for a script substitution it does not
		// explicitly vouch for, which is where Chinese lands.
		"serbian scripts read as one": {"sr-Cyrl", "sr-Latn", true},

		"chinese scripts":       {"zh-Hans", "zh-Hant", false},
		"nynorsk for bokmal":    {"nb", "nn", false},
		"danish for norwegian":  {"nb", "da", false},
		"spanish for catalan":   {"ca", "es", false},
		"swedish for norwegian": {"nb", "sv", false},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ref := &Stream{StreamType: StreamTypeSubtitle, LanguageTag: tc.refTag}
			cand := &Stream{ID: 7, StreamType: StreamTypeSubtitle, LanguageTag: tc.candTag}
			got := MatchSubtitle(ref, nil, []*Stream{cand}, defaultFloor)
			if tc.admit != (got != nil) {
				t.Errorf("MatchSubtitle(%s, [%s], %v) admitted=%v, want %v",
					tc.refTag, tc.candTag, defaultFloor, got != nil, tc.admit)
			}
		})
	}
}

// TestSubtitleFloorsAreReachableInOrder confirms each documented setting opens
// exactly the pair it is documented to open, and nothing beyond it.
func TestSubtitleFloorsAreReachableInOrder(t *testing.T) {
	t.Parallel()
	// The first floor at which each pair becomes acceptable.
	firstAdmitting := map[string]struct {
		refTag, candTag string
		floor           langtag.Tier
	}{
		"same tag":        {"nb", "nb", langtag.TierIdentical},
		"macrolanguage":   {"nb", "no", langtag.TierSameLanguage},
		"chinese scripts": {"zh-Hans", "zh-Hant", langtag.TierOtherScript},
		"serbian scripts": {"sr-Cyrl", "sr-Latn", langtag.TierSameLanguage},
		"nynorsk":         {"nb", "nn", langtag.TierIntelligible},
		"catalan":         {"ca", "es", langtag.TierSharedLiteracy},
	}
	floors := []langtag.Tier{
		langtag.TierIdentical, langtag.TierSameLanguage, langtag.TierOtherScript,
		langtag.TierIntelligible, langtag.TierSharedLiteracy,
	}
	for name, tc := range firstAdmitting {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			ref := &Stream{StreamType: StreamTypeSubtitle, LanguageTag: tc.refTag}
			cand := &Stream{ID: 7, StreamType: StreamTypeSubtitle, LanguageTag: tc.candTag}
			for _, floor := range floors {
				admitted := MatchSubtitle(ref, nil, []*Stream{cand}, floor) != nil
				want := floor >= tc.floor
				if admitted != want {
					t.Errorf("MatchSubtitle(%s, [%s], %v) admitted=%v, want %v (first admitting floor is %v)",
						tc.refTag, tc.candTag, floor, admitted, want, tc.floor)
				}
			}
		})
	}
}

// TestNoMatchMeansNoMatch pins that a reference with no acceptable candidate
// yields nil on both paths at every floor, which is what makes the caller leave
// the episode untouched. There is no fallback to a default track, no
// approximation, and no write.
func TestNoMatchMeansNoMatch(t *testing.T) {
	t.Parallel()
	floors := []langtag.Tier{
		langtag.TierIdentical, langtag.TierSameLanguage, langtag.TierOtherScript,
		langtag.TierIntelligible, langtag.TierSharedLiteracy,
	}
	// A Norwegian reference against an episode carrying nothing Norwegian.
	audRef := &Stream{StreamType: StreamTypeAudio, LanguageTag: "nb"}
	subRef := &Stream{StreamType: StreamTypeSubtitle, LanguageTag: "nb"}
	audCands := []*Stream{
		{ID: 1, StreamType: StreamTypeAudio, LanguageTag: "en"},
		{ID: 2, StreamType: StreamTypeAudio, LanguageTag: "sv"},
		{ID: 3, StreamType: StreamTypeAudio, LanguageTag: "ja"},
	}
	subCands := []*Stream{
		{ID: 4, StreamType: StreamTypeSubtitle, LanguageTag: "en"},
		{ID: 5, StreamType: StreamTypeSubtitle, LanguageTag: "sv"},
	}
	if got := MatchAudio(audRef, audCands); got != nil {
		t.Errorf("MatchAudio(nb, [en sv ja]) = ID %d, want nil", got.ID)
	}
	for _, floor := range floors {
		if got := MatchSubtitle(subRef, audRef, subCands, floor); got != nil {
			t.Errorf("MatchSubtitle(nb, [en sv], %v) = ID %d, want nil", floor, got.ID)
		}
	}
}
