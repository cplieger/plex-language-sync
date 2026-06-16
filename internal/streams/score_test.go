package streams

import (
	"fmt"
	"testing"

	"pgregory.net/rapid"
)

func TestScoreAudioStream(t *testing.T) {
	tests := []struct {
		name     string
		ref, s   Stream
		wantMin  int
		wantMore bool
	}{
		{
			name:    "same codec adds 5",
			ref:     Stream{Codec: "eac3"},
			s:       Stream{Codec: "eac3"},
			wantMin: 5,
		},
		{
			name:    "different codec adds 0",
			ref:     Stream{Codec: "eac3"},
			s:       Stream{Codec: "aac"},
			wantMin: 0,
		},
		{
			name:    "same channel layout adds 3",
			ref:     Stream{AudioChannelLayout: "5.1(side)"},
			s:       Stream{AudioChannelLayout: "5.1(side)"},
			wantMin: 3,
		},
		{
			name:    "low channel ref prefers more channels",
			ref:     Stream{Channels: 2},
			s:       Stream{Channels: 6},
			wantMin: 2,
		},
		{
			name:    "high channel ref no bonus",
			ref:     Stream{Channels: 6},
			s:       Stream{Channels: 2},
			wantMin: 0,
		},
		{
			name:    "matching titles add score",
			ref:     Stream{Title: "English", DisplayTitle: "English (EAC3 5.1)"},
			s:       Stream{Title: "English", DisplayTitle: "English (EAC3 5.1)"},
			wantMin: 10,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := ScoreAudio(&tt.ref, &tt.s)
			if got < tt.wantMin {
				t.Errorf("ScoreAudio() = %d, want >= %d", got, tt.wantMin)
			}
		})
	}
}

// --- Tests: ScoreSubtitle ---

func TestScoreSubtitleStream(t *testing.T) {
	t.Run("nil ref returns 0", func(t *testing.T) {
		got := ScoreSubtitle(nil, &Stream{})
		if got != 0 {
			t.Errorf("ScoreSubtitle(nil) = %d, want 0", got)
		}
	})

	t.Run("matching forced and HI adds 6", func(t *testing.T) {
		ref := &Stream{Forced: true, HearingImpaired: true}
		s := &Stream{Forced: true, HearingImpaired: true}
		got := ScoreSubtitle(ref, s)
		if got < 6 {
			t.Errorf("ScoreSubtitle() = %d, want >= 6", got)
		}
	})

	t.Run("mismatched forced loses 3", func(t *testing.T) {
		ref := &Stream{Forced: true}
		s := &Stream{Forced: false}
		got := ScoreSubtitle(ref, s)
		// forced mismatch: no +3 for forced, but +3 for HI match (both false)
		if got > 4 {
			t.Errorf("ScoreSubtitle() = %d, want <= 4", got)
		}
	})

	t.Run("matching codec adds 1", func(t *testing.T) {
		ref := &Stream{Codec: "srt"}
		s := &Stream{Codec: "srt"}
		// +3 forced match (both false) +3 HI match (both false) +1 codec
		got := ScoreSubtitle(ref, s)
		if got != 7 {
			t.Errorf("ScoreSubtitle() = %d, want 7", got)
		}
	})
}

// --- Tests: SubtitleCriteria ---

func TestTitleMatchScore(t *testing.T) {
	t.Run("all titles match", func(t *testing.T) {
		ref := &Stream{
			Title: "English", DisplayTitle: "English (EAC3)",
			ExtendedDisplayTitle: "English (EAC3 5.1)",
		}
		s := &Stream{
			Title: "English", DisplayTitle: "English (EAC3)",
			ExtendedDisplayTitle: "English (EAC3 5.1)",
		}
		got := TitleMatchScore(ref, s)
		if got != 15 {
			t.Errorf("TitleMatchScore() = %d, want 15", got)
		}
	})

	t.Run("no titles match", func(t *testing.T) {
		ref := &Stream{Title: "English"}
		s := &Stream{Title: "Japanese"}
		got := TitleMatchScore(ref, s)
		if got != 0 {
			t.Errorf("TitleMatchScore() = %d, want 0", got)
		}
	})

	t.Run("empty titles no match", func(t *testing.T) {
		ref := &Stream{}
		s := &Stream{}
		got := TitleMatchScore(ref, s)
		if got != 0 {
			t.Errorf("TitleMatchScore() = %d, want 0", got)
		}
	})
}

// --- Tests: FilterByLanguage ---

func TestFilterByLanguage(t *testing.T) {
	streams := []*Stream{
		{ID: 1, LanguageCode: "eng"},
		{ID: 2, LanguageCode: "jpn"},
		{ID: 3, LanguageCode: "eng"},
	}
	got := FilterByLanguage(streams, "eng")
	if len(got) != 2 {
		t.Fatalf("expected 2, got %d", len(got))
	}
	if got[0].ID != 1 || got[1].ID != 3 {
		t.Errorf("unexpected IDs: %d, %d", got[0].ID, got[1].ID)
	}

	got = FilterByLanguage(streams, "kor")
	if len(got) != 0 {
		t.Errorf("expected 0 for kor, got %d", len(got))
	}
}

// --- Tests: FilterByBoolPref ---

func TestFilterByBoolPref(t *testing.T) {
	streams := []*Stream{
		{ID: 1, Forced: true},
		{ID: 2, Forced: false},
		{ID: 3, Forced: true},
	}

	t.Run("filters to matching", func(t *testing.T) {
		got := FilterByBoolPref(streams, true, func(s *Stream) bool { return s.Forced })
		if len(got) != 2 {
			t.Fatalf("expected 2, got %d", len(got))
		}
	})

	t.Run("returns all when none match", func(t *testing.T) {
		all := []*Stream{{ID: 1, Forced: false}}
		got := FilterByBoolPref(all, true, func(s *Stream) bool { return s.Forced })
		if len(got) != 1 {
			t.Fatalf("expected 1 (fallback to all), got %d", len(got))
		}
	})
}

// --- Tests: BestByScore ---

func TestBestByScore(t *testing.T) {
	streams := []*Stream{
		{ID: 1, Channels: 2},
		{ID: 2, Channels: 6},
		{ID: 3, Channels: 4},
	}
	got := BestByScore(streams, func(s *Stream) int { return s.Channels })
	if got.ID != 2 {
		t.Errorf("expected ID=2 (highest channels), got ID=%d", got.ID)
	}
}

func TestBestByScoreEmpty(t *testing.T) {
	t.Parallel()
	got := BestByScore(nil, func(s *Stream) int { return s.Channels })
	if got != nil {
		t.Errorf("BestByScore(nil) = %v, want nil", got)
	}
}

// --- Tests: FirstPartID ---

func TestScoreAudioStreamChannelPreference(t *testing.T) {
	t.Run("low channel ref prefers higher channels", func(t *testing.T) {
		ref := &Stream{Channels: 2}
		low := &Stream{Channels: 2}
		high := &Stream{Channels: 6}
		scoreLow := ScoreAudio(ref, low)
		scoreHigh := ScoreAudio(ref, high)
		if scoreHigh <= scoreLow {
			t.Errorf("6ch (%d) should score higher than 2ch (%d) for 2ch ref", scoreHigh, scoreLow)
		}
	})

	t.Run("high channel ref no bonus for lower", func(t *testing.T) {
		ref := &Stream{Channels: 8, Codec: "eac3", AudioChannelLayout: "7.1"}
		s := &Stream{Channels: 2, Codec: "aac", AudioChannelLayout: "stereo"}
		score := ScoreAudio(ref, s)
		if score != 0 {
			t.Errorf("expected 0 for lower channels with high ref and different codec/layout, got %d", score)
		}
	})
}

// --- Tests: ScoreSubtitle comprehensive ---

func TestScoreSubtitleStreamComprehensive(t *testing.T) {
	t.Run("all fields match", func(t *testing.T) {
		ref := &Stream{
			Forced: true, HearingImpaired: true, Codec: "srt",
			Title: "English", DisplayTitle: "English SDH",
			ExtendedDisplayTitle: "English SDH (SRT)",
		}
		s := &Stream{
			Forced: true, HearingImpaired: true, Codec: "srt",
			Title: "English", DisplayTitle: "English SDH",
			ExtendedDisplayTitle: "English SDH (SRT)",
		}
		got := ScoreSubtitle(ref, s)
		// Expected: forced(3) + HI(3) + codec(1) + title(5) + display(5) + extended(5).
		if got != 22 {
			t.Errorf("all match score = %d, want 22", got)
		}
	})

	t.Run("nothing matches", func(t *testing.T) {
		ref := &Stream{Forced: true, HearingImpaired: true, Codec: "srt"}
		s := &Stream{Forced: false, HearingImpaired: false, Codec: "ass"}
		got := ScoreSubtitle(ref, s)
		if got != 0 {
			t.Errorf("nothing match score = %d, want 0", got)
		}
	})
}

// --- Tests: ContainsDescriptive additional terms ---

func TestBestByScoreSingle(t *testing.T) {
	streams := []*Stream{{ID: 1}}
	got := BestByScore(streams, func(s *Stream) int { return 0 })
	if got.ID != 1 {
		t.Errorf("expected ID=1, got ID=%d", got.ID)
	}
}

// --- Tests: FilterByLanguage empty language ---

func TestFilterByLanguageEmptyCode(t *testing.T) {
	streams := []*Stream{
		{ID: 1, LanguageCode: "eng"},
		{ID: 2, LanguageCode: ""},
	}
	got := FilterByLanguage(streams, "")
	if len(got) != 1 || got[0].ID != 2 {
		t.Errorf("expected stream with empty language code, got %v", got)
	}
}

// --- Tests: TitleMatchScore partial matches ---

func TestTitleMatchScorePartial(t *testing.T) {
	ref := &Stream{
		Title:                "English",
		DisplayTitle:         "English (EAC3)",
		ExtendedDisplayTitle: "English (EAC3 5.1)",
	}
	s := &Stream{
		Title:                "English",
		DisplayTitle:         "English (AAC)",
		ExtendedDisplayTitle: "English (AAC Stereo)",
	}
	got := TitleMatchScore(ref, s)
	if got != 5 {
		t.Errorf("only Title matches, expected 5, got %d", got)
	}
}

// --- Tests: loadConfig debug mode ---

func TestScoreAudioStreamNonNegative(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ref := &Stream{
			Codec:                rapid.StringMatching(`[a-z0-9]{0,10}`).Draw(t, "ref_codec"),
			AudioChannelLayout:   rapid.StringMatching(`[a-z0-9.()]{0,15}`).Draw(t, "ref_layout"),
			Channels:             rapid.IntRange(0, 16).Draw(t, "ref_channels"),
			Title:                rapid.StringMatching(`[A-Za-z ]{0,20}`).Draw(t, "ref_title"),
			DisplayTitle:         rapid.StringMatching(`[A-Za-z ]{0,20}`).Draw(t, "ref_display"),
			ExtendedDisplayTitle: rapid.StringMatching(`[A-Za-z ]{0,30}`).Draw(t, "ref_ext"),
		}
		s := &Stream{
			Codec:                rapid.StringMatching(`[a-z0-9]{0,10}`).Draw(t, "s_codec"),
			AudioChannelLayout:   rapid.StringMatching(`[a-z0-9.()]{0,15}`).Draw(t, "s_layout"),
			Channels:             rapid.IntRange(0, 16).Draw(t, "s_channels"),
			Title:                rapid.StringMatching(`[A-Za-z ]{0,20}`).Draw(t, "s_title"),
			DisplayTitle:         rapid.StringMatching(`[A-Za-z ]{0,20}`).Draw(t, "s_display"),
			ExtendedDisplayTitle: rapid.StringMatching(`[A-Za-z ]{0,30}`).Draw(t, "s_ext"),
		}
		score := ScoreAudio(ref, s)
		if score < 0 {
			t.Errorf("ScoreAudio() = %d, want >= 0", score)
		}
	})
}

func TestScoreSubtitleStreamNonNegative(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		ref := &Stream{
			Forced:               rapid.Bool().Draw(t, "ref_forced"),
			HearingImpaired:      rapid.Bool().Draw(t, "ref_hi"),
			Codec:                rapid.StringMatching(`[a-z]{0,5}`).Draw(t, "ref_codec"),
			Title:                rapid.StringMatching(`[A-Za-z ]{0,20}`).Draw(t, "ref_title"),
			DisplayTitle:         rapid.StringMatching(`[A-Za-z ]{0,20}`).Draw(t, "ref_display"),
			ExtendedDisplayTitle: rapid.StringMatching(`[A-Za-z ]{0,30}`).Draw(t, "ref_ext"),
		}
		s := &Stream{
			Forced:               rapid.Bool().Draw(t, "s_forced"),
			HearingImpaired:      rapid.Bool().Draw(t, "s_hi"),
			Codec:                rapid.StringMatching(`[a-z]{0,5}`).Draw(t, "s_codec"),
			Title:                rapid.StringMatching(`[A-Za-z ]{0,20}`).Draw(t, "s_title"),
			DisplayTitle:         rapid.StringMatching(`[A-Za-z ]{0,20}`).Draw(t, "s_display"),
			ExtendedDisplayTitle: rapid.StringMatching(`[A-Za-z ]{0,30}`).Draw(t, "s_ext"),
		}
		score := ScoreSubtitle(ref, s)
		if score < 0 {
			t.Errorf("ScoreSubtitle() = %d, want >= 0", score)
		}
	})
}

func TestScoreAudioStreamSelfMaximal(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := &Stream{
			Codec:                rapid.StringMatching(`[a-z0-9]{1,10}`).Draw(t, "codec"),
			AudioChannelLayout:   rapid.StringMatching(`[a-z0-9.()]{1,15}`).Draw(t, "layout"),
			Channels:             rapid.IntRange(3, 16).Draw(t, "channels"),
			Title:                rapid.StringMatching(`[A-Za-z]{1,20}`).Draw(t, "title"),
			DisplayTitle:         rapid.StringMatching(`[A-Za-z]{1,20}`).Draw(t, "display"),
			ExtendedDisplayTitle: rapid.StringMatching(`[A-Za-z]{1,30}`).Draw(t, "ext"),
		}
		selfScore := ScoreAudio(s, s)
		other := &Stream{
			Codec:                rapid.StringMatching(`[a-z0-9]{1,10}`).Draw(t, "other_codec"),
			AudioChannelLayout:   rapid.StringMatching(`[a-z0-9.()]{1,15}`).Draw(t, "other_layout"),
			Channels:             rapid.IntRange(1, s.Channels).Draw(t, "other_channels"),
			Title:                rapid.StringMatching(`[A-Za-z]{1,20}`).Draw(t, "other_title"),
			DisplayTitle:         rapid.StringMatching(`[A-Za-z]{1,20}`).Draw(t, "other_display"),
			ExtendedDisplayTitle: rapid.StringMatching(`[A-Za-z]{1,30}`).Draw(t, "other_ext"),
		}
		otherScore := ScoreAudio(s, other)
		if otherScore > selfScore {
			t.Errorf("ScoreAudio(s, other)=%d > ScoreAudio(s, s)=%d",
				otherScore, selfScore)
		}
	})
}

func TestTitleMatchScoreSelfMaximal(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		s := &Stream{
			Title:                rapid.StringMatching(`[A-Za-z]{1,20}`).Draw(t, "title"),
			DisplayTitle:         rapid.StringMatching(`[A-Za-z]{1,20}`).Draw(t, "display"),
			ExtendedDisplayTitle: rapid.StringMatching(`[A-Za-z]{1,30}`).Draw(t, "ext"),
		}
		selfScore := TitleMatchScore(s, s)
		if selfScore != 15 {
			t.Errorf("TitleMatchScore(s, s) = %d, want 15 (all non-empty titles match)", selfScore)
		}
	})
}

func TestFilterByBoolPrefAllMatch(t *testing.T) {
	streams := []*Stream{
		{ID: 1, Forced: true},
		{ID: 2, Forced: true},
	}
	got := FilterByBoolPref(streams, true, func(s *Stream) bool { return s.Forced })
	if len(got) != 2 {
		t.Errorf("FilterByBoolPref all match: got %d, want 2", len(got))
	}
}

func TestFilterByBoolPrefNoneMatch(t *testing.T) {
	streams := []*Stream{
		{ID: 1, Forced: false},
		{ID: 2, Forced: false},
	}
	got := FilterByBoolPref(streams, true, func(s *Stream) bool { return s.Forced })
	// None match desired=true, so returns original list.
	if len(got) != 2 {
		t.Errorf("FilterByBoolPref none match: got %d, want 2 (fallback)", len(got))
	}
}

func TestFindSubtitleByLanguage(t *testing.T) {
	t.Parallel()
	eng := &Stream{ID: 1, StreamType: StreamTypeSubtitle, LanguageCode: "eng"}
	jpn := &Stream{ID: 2, StreamType: StreamTypeSubtitle, LanguageCode: "jpn"}
	fra := &Stream{ID: 3, StreamType: StreamTypeSubtitle, LanguageCode: "fra"}

	tests := []struct {
		name     string
		langCode string
		streams  []*Stream
		wantID   int
		wantNil  bool
	}{
		{name: "finds english", streams: []*Stream{eng, jpn, fra}, langCode: "eng", wantID: 1, wantNil: false},
		{name: "finds japanese", streams: []*Stream{eng, jpn, fra}, langCode: "jpn", wantID: 2, wantNil: false},
		{name: "finds french", streams: []*Stream{eng, jpn, fra}, langCode: "fra", wantID: 3, wantNil: false},
		{name: "not found", streams: []*Stream{eng, jpn}, langCode: "kor", wantID: 0, wantNil: true},
		{name: "empty streams", streams: nil, langCode: "eng", wantID: 0, wantNil: true},
		{name: "empty language", streams: []*Stream{eng}, langCode: "", wantID: 0, wantNil: true},
		{name: "returns first match", streams: []*Stream{
			{ID: 10, StreamType: StreamTypeSubtitle, LanguageCode: "eng"},
			{ID: 11, StreamType: StreamTypeSubtitle, LanguageCode: "eng"},
		}, langCode: "eng", wantID: 10, wantNil: false},
		{name: "prefers ASS over SRT", streams: []*Stream{
			{ID: 10, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Codec: "srt"},
			{ID: 11, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Codec: "ass"},
		}, langCode: "eng", wantID: 11, wantNil: false},
		{name: "prefers ASS over PGS", streams: []*Stream{
			{ID: 10, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Codec: "pgs"},
			{ID: 11, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Codec: "ass"},
		}, langCode: "eng", wantID: 11, wantNil: false},
		{name: "prefers PGS over SRT", streams: []*Stream{
			{ID: 10, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Codec: "srt"},
			{ID: 11, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Codec: "pgs"},
		}, langCode: "eng", wantID: 11, wantNil: false},
		{name: "prefers vobsub over SRT", streams: []*Stream{
			{ID: 10, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Codec: "srt"},
			{ID: 11, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Codec: "vobsub"},
		}, langCode: "eng", wantID: 11, wantNil: false},
		{name: "unknown codec loses to SRT", streams: []*Stream{
			{ID: 10, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Codec: ""},
			{ID: 11, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Codec: "srt"},
		}, langCode: "eng", wantID: 11, wantNil: false},
		{name: "same codec picks first", streams: []*Stream{
			{ID: 10, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Codec: "srt"},
			{ID: 11, StreamType: StreamTypeSubtitle, LanguageCode: "eng", Codec: "srt"},
		}, langCode: "eng", wantID: 10, wantNil: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := FindSubtitleByLanguage(tt.streams, tt.langCode)
			if tt.wantNil {
				if got != nil {
					t.Errorf("FindSubtitleByLanguage(streams, %q) = stream %d, want nil",
						tt.langCode, got.ID)
				}
				return
			}
			if got == nil {
				t.Fatalf("FindSubtitleByLanguage(streams, %q) = nil, want stream %d",
					tt.langCode, tt.wantID)
			}
			if got.ID != tt.wantID {
				t.Errorf("FindSubtitleByLanguage(streams, %q).ID = %d, want %d",
					tt.langCode, got.ID, tt.wantID)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Property-based tests for extracted functions
// ---------------------------------------------------------------------------

func TestSubtitleCodecScore(t *testing.T) {
	t.Parallel()
	tests := []struct {
		codec string
		want  int
	}{
		{"ass", 3},
		{"ssa", 3},
		{"ASS", 3},
		{"pgs", 2},
		{"vobsub", 2},
		{"dvdsub", 2},
		{"dvb_subtitle", 2},
		{"hdmv_pgs_subtitle", 2},
		{"srt", 1},
		{"subrip", 1},
		{"mov_text", 1},
		{"webvtt", 1},
		{"", 0},
		{"unknown", 0},
	}
	for _, tt := range tests {
		t.Run(tt.codec, func(t *testing.T) {
			t.Parallel()
			got := SubtitleCodecScore(tt.codec)
			if got != tt.want {
				t.Errorf("SubtitleCodecScore(%q) = %d, want %d", tt.codec, got, tt.want)
			}
		})
	}
}

func TestFindSubtitleByLanguageNeverPanics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		nStreams := rapid.IntRange(0, 5).Draw(t, "n_streams")
		streams := make([]*Stream, nStreams)
		for i := range nStreams {
			streams[i] = &Stream{
				ID:           rapid.IntRange(1, 1000).Draw(t, fmt.Sprintf("id_%d", i)),
				StreamType:   3,
				LanguageCode: rapid.StringMatching(`[a-z]{0,3}`).Draw(t, fmt.Sprintf("lang_%d", i)),
			}
		}
		langCode := rapid.StringMatching(`[a-z]{0,3}`).Draw(t, "target_lang")
		result := FindSubtitleByLanguage(streams, langCode)
		// Invariant: if result is non-nil, its language must match.
		if result != nil && result.LanguageCode != langCode {
			t.Errorf("FindSubtitleByLanguage returned stream with lang %q, want %q",
				result.LanguageCode, langCode)
		}
	})
}

// ---------------------------------------------------------------------------
// Tests for drainBody and other small functions (Round 2)
// ---------------------------------------------------------------------------

func TestFindSubtitleByLanguage_ReturnsHighestCodecScorePBT(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		codecs := []string{"srt", "ass", "ssa", "pgs", "vobsub", "webvtt", "mov_text", ""}
		n := rapid.IntRange(1, 10).Draw(t, "n")
		candidates := make([]*Stream, n)
		for i := range n {
			candidates[i] = &Stream{
				ID:           i + 1,
				StreamType:   3,
				LanguageCode: rapid.SampledFrom([]string{"eng", "fra", "kor"}).Draw(t, fmt.Sprintf("lang_%d", i)),
				Codec:        rapid.SampledFrom(codecs).Draw(t, fmt.Sprintf("codec_%d", i)),
			}
		}
		targetLang := rapid.SampledFrom([]string{"eng", "fra", "kor"}).Draw(t, "target")
		got := FindSubtitleByLanguage(candidates, targetLang)
		if got == nil {
			for _, s := range candidates {
				if s.LanguageCode == targetLang {
					t.Errorf("FindSubtitleByLanguage returned nil but candidate ID=%d has language %q", s.ID, targetLang)
				}
			}
			return
		}
		if got.LanguageCode != targetLang {
			t.Errorf("returned stream lang=%q, want %q", got.LanguageCode, targetLang)
		}
		gotScore := SubtitleCodecScore(got.Codec)
		for _, s := range candidates {
			if s.LanguageCode != targetLang {
				continue
			}
			if SubtitleCodecScore(s.Codec) > gotScore {
				t.Errorf("FindSubtitleByLanguage picked codec=%q (score=%d) but candidate ID=%d codec=%q has score=%d",
					got.Codec, gotScore, s.ID, s.Codec, SubtitleCodecScore(s.Codec))
			}
		}
	})
}

func TestFilterByLanguage_InvariantPBT(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		langs := []string{"eng", "jpn", "kor", "fra", ""}
		n := rapid.IntRange(0, 12).Draw(t, "n")
		streams := make([]*Stream, n)
		for i := range n {
			streams[i] = &Stream{
				ID:           i + 1,
				LanguageCode: rapid.SampledFrom(langs).Draw(t, fmt.Sprintf("lang_%d", i)),
			}
		}
		target := rapid.SampledFrom(langs).Draw(t, "target")
		got := FilterByLanguage(streams, target)
		for _, s := range got {
			if s.LanguageCode != target {
				t.Errorf("FilterByLanguage(%q): returned stream ID=%d has lang=%q", target, s.ID, s.LanguageCode)
			}
		}
		expected := 0
		for _, s := range streams {
			if s.LanguageCode == target {
				expected++
			}
		}
		if len(got) != expected {
			t.Errorf("FilterByLanguage(%q): got %d streams, want %d", target, len(got), expected)
		}
	})
}

func TestSubtitleCodecScore_OrderInvariant(t *testing.T) {
	t.Parallel()
	styled := []string{"ass", "ssa"}
	image := []string{"pgs", "vobsub", "dvdsub", "dvb_subtitle", "hdmv_pgs_subtitle"}
	plain := []string{"srt", "subrip", "mov_text", "webvtt"}
	unknown := []string{"", "wtf", "???"}

	for _, s := range styled {
		for _, i := range image {
			if SubtitleCodecScore(s) <= SubtitleCodecScore(i) {
				t.Errorf("styled %q score <= image %q score: %d vs %d",
					s, i, SubtitleCodecScore(s), SubtitleCodecScore(i))
			}
		}
	}
	for _, i := range image {
		for _, p := range plain {
			if SubtitleCodecScore(i) <= SubtitleCodecScore(p) {
				t.Errorf("image %q score <= plain %q score: %d vs %d",
					i, p, SubtitleCodecScore(i), SubtitleCodecScore(p))
			}
		}
	}
	for _, p := range plain {
		for _, u := range unknown {
			if SubtitleCodecScore(p) <= SubtitleCodecScore(u) {
				t.Errorf("plain %q score <= unknown %q score: %d vs %d",
					p, u, SubtitleCodecScore(p), SubtitleCodecScore(u))
			}
		}
	}
}

// --- Tests: findEpisodeReference (ops-6 wrapper) ---

// ---------------------------------------------------------------------------
// Mutation-killing boundary tests (gremlins live mutants)
// ---------------------------------------------------------------------------

// TestScoreAudio_PreferMoreChannelsBoundaries pins the exact comparator
// boundaries of the prefer_more_channels rule (score.go L34/L35). The rule
// adds 2 only when 0 < ref.Channels < 3 and s.Channels > ref.Channels.
//
// given a reference/candidate pair sitting exactly on a comparator boundary
// when ScoreAudio is computed with no other matching fields
// then the channel bonus must NOT apply (score stays 0).
func TestScoreAudio_PreferMoreChannelsBoundaries(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name        string
		refChannels int
		sChannels   int
		want        int
	}{
		// ref.Channels==0 must fail the `ref.Channels > 0` guard. A `>=`
		// mutation would let it through and add 2.
		{name: "ref channels at zero, candidate higher", refChannels: 0, sChannels: 2, want: 0},
		// ref.Channels==3 must fail the `ref.Channels < 3` guard. A `<=`
		// mutation would let it through and add 2.
		{name: "ref channels at upper limit, candidate higher", refChannels: 3, sChannels: 4, want: 0},
		// Sanity anchor: 2 channels ref with a richer candidate DOES earn
		// the bonus, so the rule is genuinely exercised by the boundary
		// cases above (not dead code).
		{name: "ref channels in range, candidate higher earns bonus", refChannels: 2, sChannels: 6, want: 2},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			ref := &Stream{Channels: tc.refChannels}
			s := &Stream{Channels: tc.sChannels}

			got := ScoreAudio(ref, s)

			if got != tc.want {
				t.Errorf("ScoreAudio(ref.Channels=%d, s.Channels=%d) = %d, want %d",
					tc.refChannels, tc.sChannels, got, tc.want)
			}
		})
	}
}

// TestBestByScore_SelectsHighestAmongNegativeScores pins the bestScore
// sentinel initialiser (score.go L196: `bestScore := -1`). When every
// candidate scores below zero except one at exactly 0, the sentinel must
// start strictly below the lowest possible score so the real maximum still
// wins. An INVERT_NEGATIVES (`-1`→`1`) or ARITHMETIC_BASE mutation pushes the
// sentinel to >= 0, so the score-0 stream at index 1 never overtakes the
// default index 0.
func TestBestByScore_SelectsHighestAmongNegativeScores(t *testing.T) {
	t.Parallel()
	streams := []*Stream{{ID: 1}, {ID: 2}}
	scoreFn := func(s *Stream) int {
		if s.ID == 1 {
			return -10
		}
		return 0
	}

	got := BestByScore(streams, scoreFn)

	if got == nil || got.ID != 2 {
		t.Errorf("BestByScore with scores [-10, 0] = %v, want stream ID=2 (the 0-score max)", got)
	}
}

// TestBestByScore_TieGoesToEarliest pins the strict `>` comparator
// (score.go L199). Documented contract: "Ties go to the earlier entry." A
// `>`→`>=` mutation would let a later equal-scoring stream overwrite the
// earlier one.
func TestBestByScore_TieGoesToEarliest(t *testing.T) {
	t.Parallel()
	streams := []*Stream{{ID: 1}, {ID: 2}}
	scoreFn := func(_ *Stream) int { return 5 }

	got := BestByScore(streams, scoreFn)

	if got == nil || got.ID != 1 {
		t.Errorf("BestByScore with tied scores = %v, want stream ID=1 (earliest on tie)", got)
	}
}
