package streams

import "testing"

// TestMatchAudioStream_VIPreferNotRequire pins the "prefer, not require"
// contract at the MatchAudio boundary: when the reference is VisualImpaired
// but NO candidate is VI, FilterByBoolPref falls back to the full list
// rather than returning nil, so the VI user still gets a track.
// FilterByBoolPref's fallback is unit-tested directly, but no test pins
// that MatchAudio preserves it, so a refactor that inlined an exact VI
// filter returning nil on empty would regress a VI user silently and slip
// past the FilterByBoolPref unit test.
func TestMatchAudioStream_VIPreferNotRequire(t *testing.T) {
	ref := &Stream{ID: 10, StreamType: StreamTypeAudio, LanguageCode: "eng", VisualImpaired: true}
	candidates := []*Stream{
		{ID: 1, StreamType: StreamTypeAudio, LanguageCode: "eng", VisualImpaired: false, Codec: "aac"},
		{ID: 2, StreamType: StreamTypeAudio, LanguageCode: "eng", VisualImpaired: false, Codec: "eac3"},
	}
	got := MatchAudio(ref, candidates)
	if got == nil {
		t.Fatal("VI ref with no VI candidate must still return a track (prefer, not require), got nil")
	}
	if got.VisualImpaired {
		t.Errorf("no candidate is VI, but MatchAudio returned VI-marked ID=%d", got.ID)
	}
}

// TestMatchSubtitleStream_HIPreferNotRequire is the subtitle analog: an HI
// reference with no HI candidate must still return a (non-HI) subtitle via
// FilterByBoolPref's fallback, not nil.
func TestMatchSubtitleStream_HIPreferNotRequire(t *testing.T) {
	ref := &Stream{ID: 10, StreamType: StreamTypeSubtitle, LanguageCode: "eng", HearingImpaired: true}
	candidates := []*Stream{
		{ID: 1, StreamType: StreamTypeSubtitle, LanguageCode: "eng", HearingImpaired: false, Codec: "srt"},
		{ID: 2, StreamType: StreamTypeSubtitle, LanguageCode: "eng", HearingImpaired: false, Codec: "ass"},
	}
	got := MatchSubtitle(ref, nil, candidates)
	if got == nil {
		t.Fatal("HI ref with no HI candidate must still return a subtitle (prefer, not require), got nil")
	}
	if got.HearingImpaired {
		t.Errorf("no candidate is HI, but MatchSubtitle returned HI-marked ID=%d", got.ID)
	}
}
