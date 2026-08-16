package streams

import (
	"testing"

	"github.com/cplieger/langtag"
)

// sub builds a subtitle stream with the language fields Plex actually supplies.
func sub(id int, code, tag string) *Stream {
	return &Stream{ID: id, StreamType: StreamTypeSubtitle, LanguageCode: code, LanguageTag: tag}
}

// aud builds an audio stream with the language fields Plex actually supplies.
func aud(id int, code, tag string) *Stream {
	return &Stream{ID: id, StreamType: StreamTypeAudio, LanguageCode: code, LanguageTag: tag}
}

// TestMatchSubtitleFixesReportedBug is the regression test for the issue that
// started this work. A viewer selects a Norwegian Bokmål subtitle on one
// episode; the next episode carries only a track Plex labels "Norwegian". The
// codes differ (nob against nor) so exact comparison found nothing and the
// episode was left alone.
func TestMatchSubtitleFixesReportedBug(t *testing.T) {
	t.Parallel()
	ref := sub(10, "nob", "nb")
	candidates := []*Stream{sub(1, "eng", "en"), sub(2, "nor", "no")}

	got := MatchSubtitle(ref, nil, candidates, langtag.TierSameLanguage)
	if got == nil {
		t.Fatal("MatchSubtitle(nob ref, [eng nor], same-language) = nil, want the nor track")
	}
	if got.ID != 2 {
		t.Errorf("MatchSubtitle(nob ref, [eng nor], same-language) = ID %d, want ID 2 (nor)", got.ID)
	}

	// The old behavior is still available, and still finds nothing, which is
	// what makes the default a deliberate choice rather than an accident.
	if got := MatchSubtitle(ref, nil, candidates, langtag.TierIdentical); got != nil {
		t.Errorf("MatchSubtitle at the identical floor = ID %d, want nil", got.ID)
	}
}

// TestMatchSubtitlePrefersExactOverSubstitution guards the other half: a graded
// match must never displace a track that matches outright.
func TestMatchSubtitlePrefersExactOverSubstitution(t *testing.T) {
	t.Parallel()
	ref := sub(10, "nob", "nb")
	candidates := []*Stream{sub(1, "nor", "no"), sub(2, "nob", "nb"), sub(3, "nno", "nn")}

	got := MatchSubtitle(ref, nil, candidates, langtag.TierIntelligible)
	if got == nil || got.ID != 2 {
		t.Fatalf("MatchSubtitle(nob ref, [nor nob nno]) = %v, want ID 2 (the exact nob track)", got)
	}
}

// TestMatchSubtitleDistinguishesRegionalSpanish covers the defect found while
// probing a live Plex server: one movie carries both a European and a Latin
// American Spanish subtitle, and both report languageCode="spa". Matching on
// that field alone cannot tell them apart, so the choice fell through to codec
// and title scoring, effectively at random. languageTag separates them.
func TestMatchSubtitleDistinguishesRegionalSpanish(t *testing.T) {
	t.Parallel()
	candidates := []*Stream{
		sub(1, "spa", "es-419"),
		sub(2, "spa", "es-ES"),
	}
	for _, tc := range []struct {
		refTag string
		wantID int
	}{
		{"es-ES", 2},
		{"es-419", 1},
	} {
		t.Run(tc.refTag, func(t *testing.T) {
			t.Parallel()
			ref := sub(10, "spa", tc.refTag)
			got := MatchSubtitle(ref, nil, candidates, langtag.TierSameLanguage)
			if got == nil {
				t.Fatalf("MatchSubtitle(%s ref, [es-419 es-ES]) = nil, want ID %d", tc.refTag, tc.wantID)
			}
			if got.ID != tc.wantID {
				t.Errorf("MatchSubtitle(%s ref, [es-419 es-ES]) = ID %d, want ID %d", tc.refTag, got.ID, tc.wantID)
			}
		})
	}
}

// TestMatchSubtitleForcedFilterRunsBeforeGrading pins the ordering fix that
// three design reviewers found independently.
//
// Grading the language first collapses the candidates to the closest tier
// reached. A hard forced-only filter applied afterwards can then empty that set
// and return no subtitle at all, even though a forced track existed one tier
// further out. The episode would be left alone where the old exact-match code
// would have matched nothing either, so the bug is invisible in a diff of
// outcomes for identical languages, and appears the moment a substitution is
// available.
func TestMatchSubtitleForcedFilterRunsBeforeGrading(t *testing.T) {
	t.Parallel()
	ref := sub(10, "nob", "nb")
	ref.Forced = true

	nonForcedExact := sub(1, "nob", "nb")
	forcedFurther := sub(2, "nor", "no")
	forcedFurther.Forced = true

	got := MatchSubtitle(ref, nil, []*Stream{nonForcedExact, forcedFurther}, langtag.TierSameLanguage)
	if got == nil {
		t.Fatal("MatchSubtitle(forced nob ref, [non-forced nob, forced nor]) = nil, want the forced nor track")
	}
	if got.ID != 2 {
		t.Errorf("MatchSubtitle = ID %d, want ID 2; the forced requirement must be applied before the language grading, not after",
			got.ID)
	}
}

// TestMatchAudioAcceptsRegionalChinese pins the reason the audio floor sits at
// other-script rather than same-language.
//
// A spoken track has no script, but langtag infers one from the region, so two
// Mandarin tracks tagged zh-CN and zh-TW differ by script as an artifact of that
// inference. Both report languageCode="chi" and matched before this change, so
// flooring audio at same-language would have stopped propagating regional audio
// that propagates today.
func TestMatchAudioAcceptsRegionalChinese(t *testing.T) {
	t.Parallel()
	ref := aud(10, "chi", "zh-CN")
	candidates := []*Stream{aud(1, "eng", "en"), aud(2, "chi", "zh-TW")}

	got := MatchAudio(ref, candidates)
	if got == nil {
		t.Fatal("MatchAudio(zh-CN ref, [en zh-TW]) = nil, want the zh-TW track; regional audio matched before this change")
	}
	if got.ID != 2 {
		t.Errorf("MatchAudio(zh-CN ref, [en zh-TW]) = ID %d, want ID 2", got.ID)
	}
}

// TestMatchAudioRefusesCrossLanguage confirms the audio floor stops short of any
// cross-language substitution, however close. Danish for Norwegian is readable
// on the page and jarring out of the speakers.
func TestMatchAudioRefusesCrossLanguage(t *testing.T) {
	t.Parallel()
	ref := aud(10, "nob", "nb")
	for _, cand := range []*Stream{aud(1, "dan", "da"), aud(2, "nno", "nn"), aud(3, "swe", "sv")} {
		if got := MatchAudio(ref, []*Stream{cand}); got != nil {
			t.Errorf("MatchAudio(nob ref, [%s]) = ID %d, want nil; audio must not substitute another language",
				cand.LanguageCode, got.ID)
		}
	}
}

// TestUntaggedTracksStillMatch preserves behavior for a library with no language
// metadata at all. The old code compared raw strings, so an empty code equalled
// an empty code and a selection propagated across the show. langtag's rule that
// an unknown tag matches nothing is right for a library and wrong here, where
// the alternative is doing nothing for the whole library.
func TestUntaggedTracksStillMatch(t *testing.T) {
	t.Parallel()
	ref := aud(10, "", "")
	candidates := []*Stream{aud(1, "eng", "en"), aud(2, "", "")}

	got := MatchAudio(ref, candidates)
	if got == nil {
		t.Fatal("MatchAudio(untagged ref, [eng, untagged]) = nil, want the untagged track")
	}
	if got.ID != 2 {
		t.Errorf("MatchAudio(untagged ref, [eng, untagged]) = ID %d, want ID 2", got.ID)
	}
}

// TestUnrecognizedCodeIsNotAnAbsentCode keeps the two failure modes apart. A
// private or misspelled code names something; it must not let an untagged track
// stand in for it, and it must still match itself as the old comparison did.
func TestUnrecognizedCodeIsNotAnAbsentCode(t *testing.T) {
	t.Parallel()
	ref := sub(10, "zzz", "")
	untagged := sub(1, "", "")
	sameGarbage := sub(2, "zzz", "")

	if got := MatchSubtitle(ref, nil, []*Stream{untagged}, langtag.TierIntelligible); got != nil {
		t.Errorf("MatchSubtitle(zzz ref, [untagged]) = ID %d, want nil", got.ID)
	}
	got := MatchSubtitle(ref, nil, []*Stream{untagged, sameGarbage}, langtag.TierIntelligible)
	if got == nil || got.ID != 2 {
		t.Fatalf("MatchSubtitle(zzz ref, [untagged, zzz]) = %v, want ID 2", got)
	}
}

// TestLangPrefersTagOverCode pins the field precedence, including that an
// unparseable tag falls back to the coarser code rather than to no language.
func TestLangPrefersTagOverCode(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		code, tag string
		want      string
	}{
		"tag wins":          {"spa", "es-419", "es-419"},
		"code fills a gap":  {"nob", "", "nb"},
		"unparseable tag":   {"nob", "!!!", "nb"},
		"neither is usable": {"", "", ""},
		"unparseable both":  {"zzz", "!!!", ""},
		"tag only":          {"", "nn", "nn"},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			s := &Stream{LanguageCode: tc.code, LanguageTag: tc.tag}
			if got := s.Lang().String(); got != tc.want {
				t.Errorf("Stream{code:%q tag:%q}.Lang() = %q, want %q", tc.code, tc.tag, got, tc.want)
			}
		})
	}
}

// TestMatchTierReportsTheDistance covers the value the app logs so that a
// surprising substitution can be diagnosed without reproducing it.
func TestMatchTierReportsTheDistance(t *testing.T) {
	t.Parallel()
	cases := map[string]struct {
		ref, cand *Stream
		want      langtag.Tier
	}{
		"same tag":       {sub(1, "nob", "nb"), sub(2, "nob", "nb"), langtag.TierIdentical},
		"macrolanguage":  {sub(1, "nob", "nb"), sub(2, "nor", "no"), langtag.TierSameLanguage},
		"region only":    {sub(1, "spa", "es-ES"), sub(2, "spa", "es-419"), langtag.TierSameLanguage},
		"other script":   {sub(1, "chi", "zh-Hans"), sub(2, "chi", "zh-Hant"), langtag.TierOtherScript},
		"close language": {sub(1, "nob", "nb"), sub(2, "nno", "nn"), langtag.TierIntelligible},
		"unrelated":      {sub(1, "nob", "nb"), sub(2, "swe", "sv"), langtag.TierNone},
		"both untagged":  {sub(1, "", ""), sub(2, "", ""), langtag.TierIdentical},
	}
	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			t.Parallel()
			if got := MatchTier(tc.ref, tc.cand); got != tc.want {
				t.Errorf("MatchTier(%q, %q) = %v, want %v",
					tc.ref.LanguageTag, tc.cand.LanguageTag, got, tc.want)
			}
		})
	}
}

// TestIntentCarriesTheFinerTag pins that the reconcile plane and the
// new-episode seeding path can deliver the regional fix, not only the event
// plane. The projection used to keep languageCode alone, which cannot express a
// region, so a recorded es-ES intent would have been indistinguishable from an
// es-419 one when replayed.
func TestIntentCarriesTheFinerTag(t *testing.T) {
	t.Parallel()
	audio := aud(1, "spa", "es-ES")
	subtitle := sub(2, "spa", "es-ES")
	intent := NewIntent(audio, subtitle, 0)

	gotAudio, gotSub := intent.RefStreams()
	if gotAudio.LanguageTag != "es-ES" {
		t.Errorf("intent audio LanguageTag = %q, want %q", gotAudio.LanguageTag, "es-ES")
	}
	if gotSub.LanguageTag != "es-ES" {
		t.Errorf("intent subtitle LanguageTag = %q, want %q", gotSub.LanguageTag, "es-ES")
	}

	// Replaying the recorded intent must reach the European track, not the
	// Latin American one, which is what the event plane would have chosen.
	candidates := []*Stream{sub(3, "spa", "es-419"), sub(4, "spa", "es-ES")}
	got := MatchSubtitle(gotSub, gotAudio, candidates, langtag.TierSameLanguage)
	if got == nil || got.ID != 4 {
		t.Fatalf("replayed intent matched %v, want ID 4 (es-ES)", got)
	}
}

// TestMatchSubtitleHearingImpairedIsAPreferenceNotAFilter pins the regression
// three reviewers found independently.
//
// The forced flag is a hard requirement and is applied before the language
// grading; the hearing-impaired flag is a preference and must be applied after.
// FilterByBoolPref falls back to the whole set only when NOTHING in it matches,
// so applying it first lets one hearing-impaired track in an unrelated language
// capture the candidate set, after which the grading finds nothing acceptable in
// it and returns no subtitle at all. That is worse than the plain string
// equality this change replaced.
func TestMatchSubtitleHearingImpairedIsAPreferenceNotAFilter(t *testing.T) {
	t.Parallel()

	t.Run("a foreign hearing-impaired track must not capture the set", func(t *testing.T) {
		t.Parallel()
		ref := sub(10, "eng", "en")
		ref.HearingImpaired = true

		exactNonHI := sub(1, "eng", "en")
		foreignHI := sub(2, "spa", "es")
		foreignHI.HearingImpaired = true

		// At the strictest floor this is the exact behavior of the old
		// string comparison, so it must not regress.
		for _, floor := range []langtag.Tier{
			langtag.TierIdentical, langtag.TierSameLanguage,
			langtag.TierOtherScript, langtag.TierIntelligible,
		} {
			got := MatchSubtitle(ref, nil, []*Stream{exactNonHI, foreignHI}, floor)
			if got == nil {
				t.Errorf("MatchSubtitle(eng+HI ref, [eng non-HI, spa HI], %v) = nil, want the eng track", floor)
				continue
			}
			if got.ID != 1 {
				t.Errorf("MatchSubtitle(eng+HI ref, [eng non-HI, spa HI], %v) = ID %d, want ID 1 (language outranks the HI preference)",
					floor, got.ID)
			}
		}
	})

	t.Run("the preference still applies within the chosen language", func(t *testing.T) {
		t.Parallel()
		ref := sub(10, "eng", "en")
		ref.HearingImpaired = true

		nonHI := sub(1, "eng", "en")
		hi := sub(2, "eng", "en")
		hi.HearingImpaired = true

		got := MatchSubtitle(ref, nil, []*Stream{nonHI, hi}, langtag.TierIdentical)
		if got == nil || got.ID != 2 {
			t.Fatalf("MatchSubtitle(eng+HI ref, [eng non-HI, eng HI]) = %v, want ID 2", got)
		}
	})

	t.Run("a regional variant must not lose to a farther hearing-impaired track", func(t *testing.T) {
		t.Parallel()
		ref := sub(10, "spa", "es-ES")
		ref.HearingImpaired = true

		exactNonHI := sub(1, "spa", "es-ES")
		regionalHI := sub(2, "spa", "es-419")
		regionalHI.HearingImpaired = true

		got := MatchSubtitle(ref, nil, []*Stream{exactNonHI, regionalHI}, langtag.TierOtherScript)
		if got == nil || got.ID != 1 {
			t.Fatalf("MatchSubtitle(es-ES+HI ref, [es-ES non-HI, es-419 HI]) = %v, want ID 1; the closer language wins and the HI preference applies within it",
				got)
		}
	})
}
