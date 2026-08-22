package streams

import (
	"fmt"
	"testing"

	"github.com/cplieger/langtag/v2"
	"github.com/cplieger/plexapi/v2"
	"pgregory.net/rapid"
)

func TestMatchAudioStream(t *testing.T) {
	tests := []struct {
		name       string
		ref        *Stream
		candidates []*Stream
		wantID     int
	}{
		{
			name: "nil ref returns nil",
			ref:  nil,
			candidates: []*Stream{
				{ID: 1, StreamType: StreamTypeAudio, LanguageCode: "eng"},
			},
			wantID: 0,
		},
		{
			name: "exact language match single",
			ref:  &Stream{ID: 10, StreamType: StreamTypeAudio, LanguageCode: "jpn", Codec: "aac"},
			candidates: []*Stream{
				{ID: 1, StreamType: StreamTypeAudio, LanguageCode: "eng", Codec: "aac"},
				{ID: 2, StreamType: StreamTypeAudio, LanguageCode: "jpn", Codec: "aac"},
			},
			wantID: 2,
		},
		{
			name: "no language match returns nil",
			ref:  &Stream{ID: 10, StreamType: StreamTypeAudio, LanguageCode: "kor"},
			candidates: []*Stream{
				{ID: 1, StreamType: StreamTypeAudio, LanguageCode: "eng"},
				{ID: 2, StreamType: StreamTypeAudio, LanguageCode: "jpn"},
			},
			wantID: 0,
		},
		{
			name: "prefers matching codec",
			ref:  &Stream{ID: 10, StreamType: StreamTypeAudio, LanguageCode: "eng", Codec: "eac3"},
			candidates: []*Stream{
				{ID: 1, StreamType: StreamTypeAudio, LanguageCode: "eng", Codec: "aac"},
				{ID: 2, StreamType: StreamTypeAudio, LanguageCode: "eng", Codec: "eac3"},
			},
			wantID: 2,
		},
		{
			name: "prefers matching channel layout",
			ref: &Stream{
				ID: 10, StreamType: StreamTypeAudio, LanguageCode: "eng",
				Codec: "aac", AudioChannelLayout: "5.1(side)",
			},
			candidates: []*Stream{
				{ID: 1, StreamType: StreamTypeAudio, LanguageCode: "eng", Codec: "aac", AudioChannelLayout: "stereo"},
				{ID: 2, StreamType: StreamTypeAudio, LanguageCode: "eng", Codec: "aac", AudioChannelLayout: "5.1(side)"},
			},
			wantID: 2,
		},
		{
			name: "filters out visual impaired when ref is not",
			ref:  &Stream{ID: 10, StreamType: StreamTypeAudio, LanguageCode: "eng", VisualImpaired: false},
			candidates: []*Stream{
				{ID: 1, StreamType: StreamTypeAudio, LanguageCode: "eng", VisualImpaired: true},
				{ID: 2, StreamType: StreamTypeAudio, LanguageCode: "eng", VisualImpaired: false},
			},
			wantID: 2,
		},
		{
			name: "prefers visual impaired when ref is",
			ref:  &Stream{ID: 10, StreamType: StreamTypeAudio, LanguageCode: "eng", VisualImpaired: true},
			candidates: []*Stream{
				{ID: 1, StreamType: StreamTypeAudio, LanguageCode: "eng", VisualImpaired: false},
				{ID: 2, StreamType: StreamTypeAudio, LanguageCode: "eng", VisualImpaired: true},
			},
			wantID: 2,
		},
		{
			name: "filters descriptive tracks",
			ref:  &Stream{ID: 10, StreamType: StreamTypeAudio, LanguageCode: "eng", Title: "English"},
			candidates: []*Stream{
				{ID: 1, StreamType: StreamTypeAudio, LanguageCode: "eng", Title: "English (Commentary)"},
				{ID: 2, StreamType: StreamTypeAudio, LanguageCode: "eng", Title: "English"},
			},
			wantID: 2,
		},
		{
			name: "title match boosts score",
			ref: &Stream{
				ID: 10, StreamType: StreamTypeAudio, LanguageCode: "eng",
				DisplayTitle: "English (EAC3 5.1)",
			},
			candidates: []*Stream{
				{ID: 1, StreamType: StreamTypeAudio, LanguageCode: "eng", DisplayTitle: "English (AAC Stereo)"},
				{ID: 2, StreamType: StreamTypeAudio, LanguageCode: "eng", DisplayTitle: "English (EAC3 5.1)"},
			},
			wantID: 2,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchAudio(tt.ref, tt.candidates)
			gotID := 0
			if got != nil {
				gotID = int(got.ID)
			}
			if gotID != tt.wantID {
				t.Errorf("MatchAudio() got ID=%d, want ID=%d", gotID, tt.wantID)
			}
		})
	}
}

func TestMatchSubtitleStream(t *testing.T) {
	tests := []struct {
		name       string
		ref        *Stream
		candidates []*Stream
		wantID     int
	}{
		{
			name: "nil ref returns nil",
			ref:  nil,
			candidates: []*Stream{
				{ID: 1, StreamType: StreamTypeSubtitle, LanguageCode: "eng"},
			},
			wantID: 0,
		},
		{
			name: "nil ref never matches anything, forced candidates included (no subtitle means no subtitle)",
			ref:  nil,
			candidates: []*Stream{
				{ID: 1, StreamType: StreamTypeSubtitle, LanguageCode: "jpn", Forced: false},
				{ID: 2, StreamType: StreamTypeSubtitle, LanguageCode: "jpn", Forced: true},
				{ID: 3, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Forced: true},
			},
			wantID: 0,
		},
		{
			name: "exact language match",
			ref:  &Stream{ID: 10, StreamType: StreamTypeSubtitle, LanguageCode: "eng"},
			candidates: []*Stream{
				{ID: 1, StreamType: StreamTypeSubtitle, LanguageCode: "jpn"},
				{ID: 2, StreamType: StreamTypeSubtitle, LanguageCode: "eng"},
			},
			wantID: 2,
		},
		{
			name: "prefers hearing impaired when ref is",
			ref: &Stream{
				ID: 10, StreamType: StreamTypeSubtitle, LanguageCode: "eng",
				HearingImpaired: true,
			},
			candidates: []*Stream{
				{ID: 1, StreamType: StreamTypeSubtitle, LanguageCode: "eng", HearingImpaired: false},
				{ID: 2, StreamType: StreamTypeSubtitle, LanguageCode: "eng", HearingImpaired: true},
			},
			wantID: 2,
		},
		{
			name: "no match returns nil",
			ref:  &Stream{ID: 10, StreamType: StreamTypeSubtitle, LanguageCode: "kor"},
			candidates: []*Stream{
				{ID: 1, StreamType: StreamTypeSubtitle, LanguageCode: "eng"},
			},
			wantID: 0,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchSubtitle(tt.ref, tt.candidates, langtag.TierIdentical)
			gotID := 0
			if got != nil {
				gotID = int(got.ID)
			}
			if gotID != tt.wantID {
				t.Errorf("MatchSubtitle() got ID=%d, want ID=%d", gotID, tt.wantID)
			}
		})
	}
}

func TestSubtitleMatchCriteria(t *testing.T) {
	// The old two-case split ("nil ref nil audio" / "nil ref with audio")
	// parameterized on a reference-audio argument SubtitleCriteria no longer
	// takes. "A nil ref must not search for forced subs in the audio language"
	// is now structural — the function cannot see an audio stream — so one nil
	// case covers the policy.
	t.Run("nil ref yields no criteria (no subtitle means no subtitle)", func(t *testing.T) {
		got, ok := SubtitleCriteria(nil)
		if ok {
			t.Errorf("SubtitleCriteria(nil) = (%+v, true), want ok=false", got)
		}
	})

	t.Run("criteria come from the reference subtitle's language and flags", func(t *testing.T) {
		ref := &Stream{LanguageCode: "eng", Forced: false, HearingImpaired: true}
		got, ok := SubtitleCriteria(ref)
		if !ok {
			t.Fatal("SubtitleCriteria(ref) ok = false, want true")
		}
		if got.Lang.Language() != "en" || got.ForcedOnly || !got.HearingImpairedOnly {
			t.Errorf("SubtitleCriteria(ref) = {lang %q forced %v hi %v}, want {en false true}",
				got.Lang.Language(), got.ForcedOnly, got.HearingImpairedOnly)
		}
	})
}

func TestMatchSubtitleStreamNilRefReturnsNil(t *testing.T) {
	candidates := []*Stream{
		{ID: 1, StreamType: StreamTypeSubtitle, LanguageCode: "jpn", Forced: true, Codec: "srt"},
		{ID: 2, StreamType: StreamTypeSubtitle, LanguageCode: "jpn", Forced: true, Codec: "ass"},
		{ID: 3, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Forced: true},
	}
	// Forced same-language candidates are present precisely so a nil ref that
	// leaked the audio language into the criteria would select one; it must not.
	got := MatchSubtitle(nil, candidates, langtag.TierIdentical)
	if got != nil {
		t.Errorf("nil ref must always return nil (no subtitle means no subtitle), got ID=%d", got.ID)
	}
}

func TestMatchSubtitleStreamNoLanguageMatch(t *testing.T) {
	ref := &Stream{ID: 10, StreamType: StreamTypeSubtitle, LanguageCode: "kor"}
	candidates := []*Stream{
		{ID: 1, StreamType: StreamTypeSubtitle, LanguageCode: "eng"},
		{ID: 2, StreamType: StreamTypeSubtitle, LanguageCode: "jpn"},
	}
	got := MatchSubtitle(ref, candidates, langtag.TierIdentical)
	if got != nil {
		t.Errorf("expected nil for no language match, got ID=%d", got.ID)
	}
}

func TestMatchSubtitleStreamHIOnly(t *testing.T) {
	ref := &Stream{
		ID: 10, StreamType: StreamTypeSubtitle, LanguageCode: "eng",
		HearingImpaired: true,
	}
	candidates := []*Stream{
		{ID: 1, StreamType: StreamTypeSubtitle, LanguageCode: "eng", HearingImpaired: false},
		{ID: 2, StreamType: StreamTypeSubtitle, LanguageCode: "eng", HearingImpaired: true},
		{ID: 3, StreamType: StreamTypeSubtitle, LanguageCode: "eng", HearingImpaired: true, Codec: "srt"},
	}
	got := MatchSubtitle(ref, candidates, langtag.TierIdentical)
	if got == nil {
		t.Fatal("expected a match")
	}
	if !got.HearingImpaired {
		t.Errorf("expected HI subtitle, got ID=%d", got.ID)
	}
}

func TestShouldSkipSubtitleForCommentary(t *testing.T) {
	t.Run("nil refAudio returns false", func(t *testing.T) {
		if ShouldSkipSubtitleForCommentary(nil, nil) {
			t.Error("expected false for nil refAudio")
		}
	})

	t.Run("non-commentary refAudio returns false", func(t *testing.T) {
		ref := &Stream{ID: 1, LanguageCode: "eng", Title: "English"}
		targets := []*Stream{
			{ID: 2, LanguageCode: "eng", Title: "English"},
		}
		if ShouldSkipSubtitleForCommentary(ref, targets) {
			t.Error("expected false for non-commentary audio")
		}
	})

	t.Run("commentary with matching target returns false", func(t *testing.T) {
		ref := &Stream{ID: 1, LanguageCode: "eng", Title: "English (Commentary)"}
		targets := []*Stream{
			{ID: 2, LanguageCode: "eng", Title: "English (Commentary)"},
		}
		if ShouldSkipSubtitleForCommentary(ref, targets) {
			t.Error("expected false when target has matching commentary track")
		}
	})

	t.Run("commentary without any language match returns true", func(t *testing.T) {
		ref := &Stream{ID: 1, LanguageCode: "eng", Title: "English (Commentary)"}
		targets := []*Stream{
			{ID: 2, LanguageCode: "jpn", Title: "Japanese"},
		}
		if !ShouldSkipSubtitleForCommentary(ref, targets) {
			t.Error("expected true when target has no audio in ref language")
		}
	})

	t.Run("commentary with same language match returns false", func(t *testing.T) {
		// MatchAudio matches by language, so same-language non-commentary
		// track still counts as a match — subtitle changes proceed.
		ref := &Stream{ID: 1, LanguageCode: "eng", Title: "English (Commentary)"}
		targets := []*Stream{
			{ID: 2, LanguageCode: "eng", Title: "English"},
		}
		if ShouldSkipSubtitleForCommentary(ref, targets) {
			t.Error("expected false when target has same-language audio")
		}
	})

	t.Run("descriptive audio without any language match returns true", func(t *testing.T) {
		ref := &Stream{ID: 1, LanguageCode: "eng", ExtendedDisplayTitle: "Audio Description"}
		targets := []*Stream{
			{ID: 2, LanguageCode: "jpn", ExtendedDisplayTitle: "Japanese (AAC Stereo)"},
		}
		if !ShouldSkipSubtitleForCommentary(ref, targets) {
			t.Error("expected true for descriptive audio without language match")
		}
	})
}

func TestMatchAudioStreamSingleCandidate(t *testing.T) {
	ref := &Stream{ID: 10, StreamType: StreamTypeAudio, LanguageCode: "eng", Codec: "aac"}
	candidates := []*Stream{
		{ID: 1, StreamType: StreamTypeAudio, LanguageCode: "eng", Codec: "eac3"},
	}
	got := MatchAudio(ref, candidates)
	if got == nil || got.ID != 1 {
		t.Errorf("single candidate should be returned, got %v", got)
	}
}

func TestMatchAudioStreamDescriptiveFiltering(t *testing.T) {
	ref := &Stream{
		ID: 10, StreamType: StreamTypeAudio, LanguageCode: "eng",
		ExtendedDisplayTitle: "English (AAC Stereo)",
	}
	candidates := []*Stream{
		{ID: 1, StreamType: StreamTypeAudio, LanguageCode: "eng", ExtendedDisplayTitle: "English (Commentary)"},
		{ID: 2, StreamType: StreamTypeAudio, LanguageCode: "eng", ExtendedDisplayTitle: "English (AAC Stereo)"},
	}
	got := MatchAudio(ref, candidates)
	if got == nil || got.ID != 2 {
		t.Errorf("should prefer non-commentary track, got ID=%d", got.ID)
	}
}

func TestMatchAudioStreamEmptyCandidates(t *testing.T) {
	ref := &Stream{ID: 10, StreamType: StreamTypeAudio, LanguageCode: "eng"}
	got := MatchAudio(ref, nil)
	if got != nil {
		t.Error("expected nil for empty candidates")
	}
}

// --- Tests: visual / hearing-impaired preference ---

func TestMatchAudioStreamVisualImpairedPreference(t *testing.T) {
	t.Run("VI ref prefers VI candidate", func(t *testing.T) {
		ref := &Stream{ID: 10, StreamType: StreamTypeAudio, LanguageCode: "eng", VisualImpaired: true}
		candidates := []*Stream{
			{ID: 1, StreamType: StreamTypeAudio, LanguageCode: "eng", VisualImpaired: false},
			{ID: 2, StreamType: StreamTypeAudio, LanguageCode: "eng", VisualImpaired: true},
		}
		got := MatchAudio(ref, candidates)
		if got == nil || got.ID != 2 {
			t.Errorf("expected VI track ID=2, got %v", got)
		}
	})

	t.Run("non-VI ref filters out VI", func(t *testing.T) {
		ref := &Stream{ID: 10, StreamType: StreamTypeAudio, LanguageCode: "eng", VisualImpaired: false}
		candidates := []*Stream{
			{ID: 1, StreamType: StreamTypeAudio, LanguageCode: "eng", VisualImpaired: true},
			{ID: 2, StreamType: StreamTypeAudio, LanguageCode: "eng", VisualImpaired: false},
		}
		got := MatchAudio(ref, candidates)
		if got == nil || got.ID != 2 {
			t.Errorf("expected non-VI track ID=2, got %v", got)
		}
	})
}

// TestMatchSubtitleStreamMultipleForced was deleted with MatchSubtitle's
// reference-audio parameter: it asserted that a nil subtitle ref combined with
// an audio ref does not search for forced subs in the audio language, which was
// only expressible while the function took an audio ref. The policy is now
// structural, and TestMatchSubtitleStreamNilRefReturnsNil plus the table's
// forced-candidate case still pin the nil-ref behaviour.

func TestMatchAudioStreamNeverPanics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nCandidates := rapid.IntRange(0, 5).Draw(t, "n_candidates")
		candidates := make([]*Stream, nCandidates)
		for i := range nCandidates {
			candidates[i] = &Stream{
				ID:           plexapi.FlexInt(i + 1),
				StreamType:   2,
				LanguageCode: rapid.SampledFrom([]string{"eng", "jpn", "kor", ""}).Draw(t, fmt.Sprintf("lang_%d", i)),
			}
		}
		var ref *Stream
		if rapid.Bool().Draw(t, "has_ref") {
			ref = &Stream{
				ID:           100,
				StreamType:   2,
				LanguageCode: rapid.SampledFrom([]string{"eng", "jpn", "kor", ""}).Draw(t, "ref_lang"),
			}
		}
		// Must not panic.
		MatchAudio(ref, candidates)
	})
}

func TestMatchSubtitleStreamNeverPanics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nCandidates := rapid.IntRange(0, 5).Draw(t, "n_candidates")
		candidates := make([]*Stream, nCandidates)
		for i := range nCandidates {
			candidates[i] = &Stream{
				ID:              plexapi.FlexInt(i + 1),
				StreamType:      3,
				LanguageCode:    rapid.SampledFrom([]string{"eng", "jpn", "kor", ""}).Draw(t, fmt.Sprintf("lang_%d", i)),
				Forced:          rapid.Bool().Draw(t, fmt.Sprintf("forced_%d", i)),
				HearingImpaired: rapid.Bool().Draw(t, fmt.Sprintf("hi_%d", i)),
			}
		}
		var ref *Stream
		if rapid.Bool().Draw(t, "has_ref") {
			ref = &Stream{
				StreamType:      3,
				LanguageCode:    rapid.SampledFrom([]string{"eng", "jpn", "kor", ""}).Draw(t, "ref_lang"),
				Forced:          rapid.Bool().Draw(t, "ref_forced"),
				HearingImpaired: rapid.Bool().Draw(t, "ref_hi"),
			}
		}
		MatchSubtitle(ref, candidates, langtag.TierIdentical)
	})
}

func TestMatchAudioStreamPrefersSameCodecAndLayout(t *testing.T) {
	ref := &Stream{
		ID: 10, StreamType: StreamTypeAudio, LanguageCode: "eng",
		Codec: "truehd", AudioChannelLayout: "7.1",
		Channels: 8,
	}
	candidates := []*Stream{
		{ID: 1, StreamType: StreamTypeAudio, LanguageCode: "eng", Codec: "aac", AudioChannelLayout: "stereo", Channels: 2},
		{ID: 2, StreamType: StreamTypeAudio, LanguageCode: "eng", Codec: "truehd", AudioChannelLayout: "7.1", Channels: 8},
		{ID: 3, StreamType: StreamTypeAudio, LanguageCode: "eng", Codec: "eac3", AudioChannelLayout: "5.1(side)", Channels: 6},
	}
	got := MatchAudio(ref, candidates)
	if got == nil || got.ID != 2 {
		t.Errorf("MatchAudio should prefer exact codec+layout match, got ID=%v", got)
	}
}

func TestMatchSubtitleStreamPrefersSameCodecAndFlags(t *testing.T) {
	ref := &Stream{
		ID: 10, StreamType: StreamTypeSubtitle, LanguageCode: "eng",
		Forced: false, HearingImpaired: false, Codec: "srt",
		Title: "English",
	}
	candidates := []*Stream{
		{ID: 1, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Forced: false, HearingImpaired: false, Codec: "ass", Title: "English"},
		{ID: 2, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Forced: false, HearingImpaired: false, Codec: "srt", Title: "English"},
	}
	got := MatchSubtitle(ref, candidates, langtag.TierIdentical)
	if got == nil || got.ID != 2 {
		t.Errorf("MatchSubtitle should prefer matching codec, got ID=%v", got)
	}
}

func TestMatchAudioStream_LanguageInvariantPBT(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nCandidates := rapid.IntRange(0, 8).Draw(t, "n_candidates")
		candidates := make([]*Stream, nCandidates)
		for i := range nCandidates {
			candidates[i] = &Stream{
				ID:           plexapi.FlexInt(i + 1),
				StreamType:   2,
				LanguageCode: rapid.SampledFrom([]string{"eng", "jpn", "kor", "fra", ""}).Draw(t, fmt.Sprintf("lang_%d", i)),
				Codec:        rapid.SampledFrom([]string{"aac", "eac3", "dts"}).Draw(t, fmt.Sprintf("codec_%d", i)),
			}
		}
		ref := &Stream{
			ID:           100,
			StreamType:   2,
			LanguageCode: rapid.SampledFrom([]string{"eng", "jpn", "kor", "fra", ""}).Draw(t, "ref_lang"),
		}
		got := MatchAudio(ref, candidates)
		if got != nil && got.LanguageCode != ref.LanguageCode {
			t.Errorf("MatchAudio returned stream lang=%q but ref lang=%q",
				got.LanguageCode, ref.LanguageCode)
		}
	})
}

func TestMatchSubtitleStream_LanguageInvariantPBT(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nCandidates := rapid.IntRange(0, 8).Draw(t, "n_candidates")
		candidates := make([]*Stream, nCandidates)
		for i := range nCandidates {
			candidates[i] = &Stream{
				ID:              plexapi.FlexInt(i + 1),
				StreamType:      3,
				LanguageCode:    rapid.SampledFrom([]string{"eng", "jpn", "kor", "fra"}).Draw(t, fmt.Sprintf("lang_%d", i)),
				Forced:          rapid.Bool().Draw(t, fmt.Sprintf("f_%d", i)),
				HearingImpaired: rapid.Bool().Draw(t, fmt.Sprintf("hi_%d", i)),
			}
		}
		var ref *Stream
		if rapid.Bool().Draw(t, "has_ref") {
			ref = &Stream{
				StreamType:      3,
				LanguageCode:    rapid.SampledFrom([]string{"eng", "jpn", "kor", "fra"}).Draw(t, "ref_lang"),
				Forced:          rapid.Bool().Draw(t, "ref_f"),
				HearingImpaired: rapid.Bool().Draw(t, "ref_hi"),
			}
		}
		got := MatchSubtitle(ref, candidates, langtag.TierIdentical)
		if ref == nil {
			// "No subtitle means no subtitle", whatever the candidates offer.
			// Asserted rather than skipped: the oracle's old second arm derived
			// wantLang from a reference AUDIO stream, which MatchSubtitle no
			// longer takes and never consulted, so that arm was unreachable.
			if got != nil {
				t.Errorf("MatchSubtitle(nil ref) = ID %d, want nil", got.ID)
			}
			return
		}
		if got == nil {
			return
		}
		if got.LanguageCode != ref.LanguageCode {
			t.Errorf("MatchSubtitle returned lang=%q, want %q", got.LanguageCode, ref.LanguageCode)
		}
	})
}

func TestMatchAudioStream_SelfIsAlwaysAMatch(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ref := &Stream{
			ID:                 42,
			StreamType:         2,
			LanguageCode:       rapid.SampledFrom([]string{"eng", "jpn", "kor"}).Draw(t, "lang"),
			Codec:              rapid.SampledFrom([]string{"aac", "eac3"}).Draw(t, "codec"),
			Channels:           rapid.IntRange(2, 8).Draw(t, "ch"),
			AudioChannelLayout: rapid.SampledFrom([]string{"stereo", "5.1(side)"}).Draw(t, "layout"),
		}
		worse := &Stream{
			ID:           99,
			StreamType:   2,
			LanguageCode: ref.LanguageCode,
			Codec:        "different-codec-xyz",
		}
		got := MatchAudio(ref, []*Stream{worse, ref})
		if got == nil {
			t.Errorf("MatchAudio returned nil when ref was in candidates")
			return
		}
		if got.ID != ref.ID {
			t.Errorf("MatchAudio(ref, [worse, ref]) picked ID=%d, want %d (self)", got.ID, ref.ID)
		}
	})
}

func TestMatchSubtitle_ForcedOnly(t *testing.T) {
	tests := []struct {
		name       string
		ref        *Stream
		candidates []*Stream
		wantID     int
	}{
		{
			name: "forced ref excludes non-forced candidate",
			ref:  &Stream{ID: 10, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Forced: true},
			candidates: []*Stream{
				{ID: 1, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Forced: false},
				{ID: 2, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Forced: true},
			},
			wantID: 2,
		},
		{
			name: "forced ref with no forced candidate returns nil",
			ref:  &Stream{ID: 10, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Forced: true},
			candidates: []*Stream{
				{ID: 1, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Forced: false},
				{ID: 2, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Forced: false},
			},
			wantID: 0,
		},
		{
			name: "forced ref tie-breaks among forced candidates by codec",
			ref:  &Stream{ID: 10, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Forced: true, Codec: "ass"},
			candidates: []*Stream{
				{ID: 1, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Forced: true, Codec: "srt"},
				{ID: 2, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Forced: true, Codec: "ass"},
			},
			wantID: 2,
		},
		{
			name: "forced ref ignores forced candidate in wrong language",
			ref:  &Stream{ID: 10, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Forced: true},
			candidates: []*Stream{
				{ID: 1, StreamType: StreamTypeSubtitle, LanguageCode: "jpn", Forced: true},
			},
			wantID: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := MatchSubtitle(tt.ref, tt.candidates, langtag.TierIdentical)
			gotID := 0
			if got != nil {
				gotID = int(got.ID)
			}
			if gotID != tt.wantID {
				t.Errorf("MatchSubtitle() got ID=%d, want ID=%d", gotID, tt.wantID)
			}
		})
	}
}

// TestMatchSubtitleStream_ForcedAndHIRefExcludesNonForced pins the combined
// forced+HI path in MatchSubtitle. forced is an EXACT filter and
// hearing-impaired a soft preference, so a ref that is BOTH must first drop
// every non-forced candidate (even a non-forced HI one that matches the ref's
// HI flag) and only then prefer HI among the forced survivors.
// TestMatchSubtitle_ForcedOnly uses non-HI refs and TestMatchSubtitleStreamHIOnly
// uses non-forced refs, so this interaction is otherwise unpinned even though
// both branches are statement-covered.
func TestMatchSubtitleStream_ForcedAndHIRefExcludesNonForced(t *testing.T) {
	ref := &Stream{ID: 10, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Forced: true, HearingImpaired: true}
	candidates := []*Stream{
		{ID: 1, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Forced: true, HearingImpaired: false},
		{ID: 2, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Forced: false, HearingImpaired: true},
	}
	got := MatchSubtitle(ref, candidates, langtag.TierIdentical)
	if got == nil || got.ID != 1 {
		t.Fatalf("forced+HI ref must apply the forced exact-filter and exclude the non-forced HI candidate ID=2, falling back to forced ID=1; got %v", got)
	}
}
