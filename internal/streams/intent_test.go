package streams

import (
	"encoding/json"
	"testing"

	"pgregory.net/rapid"
)

// drawIntentStream generates a random source Stream carrying every field
// the intent projection preserves.
func drawIntentStream(t *rapid.T, label string) *Stream {
	return &Stream{
		LanguageCode:         rapid.StringMatching(`[a-z]{0,3}`).Draw(t, label+"_lang"),
		Title:                rapid.StringMatching(`[ -~]{0,12}`).Draw(t, label+"_title"),
		DisplayTitle:         rapid.StringMatching(`[ -~]{0,12}`).Draw(t, label+"_display"),
		ExtendedDisplayTitle: rapid.StringMatching(`[ -~]{0,12}`).Draw(t, label+"_ext"),
		Codec:                rapid.StringMatching(`[a-z0-9]{0,6}`).Draw(t, label+"_codec"),
		AudioChannelLayout:   rapid.StringMatching(`[a-z0-9.]{0,8}`).Draw(t, label+"_layout"),
		Channels:             rapid.IntRange(0, 16).Draw(t, label+"_channels"),
		Forced:               rapid.Bool().Draw(t, label+"_forced"),
		HearingImpaired:      rapid.Bool().Draw(t, label+"_hi"),
		VisualImpaired:       rapid.Bool().Draw(t, label+"_vi"),
		// Episode-local identity fields the projection must NOT carry:
		ID:         rapid.IntRange(1, 999).Draw(t, label+"_id"),
		StreamType: StreamTypeAudio,
		Selected:   true,
	}
}

// matcherFieldsEqual compares the fields the matchers/scorers consume on a
// reference stream (see IntentStream's doc).
func matcherFieldsEqual(a, b *Stream) bool {
	return a.LanguageCode == b.LanguageCode &&
		a.Title == b.Title &&
		a.DisplayTitle == b.DisplayTitle &&
		a.ExtendedDisplayTitle == b.ExtendedDisplayTitle &&
		a.Codec == b.Codec &&
		a.AudioChannelLayout == b.AudioChannelLayout &&
		a.Channels == b.Channels &&
		a.Forced == b.Forced &&
		a.HearingImpaired == b.HearingImpaired &&
		a.VisualImpaired == b.VisualImpaired
}

// TestIntentRoundTripPreservesMatcherFields is the projection's core
// property: NewIntent → (JSON round-trip, as profiles.json would) →
// RefStreams preserves every matcher-relevant field of both streams, the
// nil-subtitle marker, and the observation timestamp — while dropping the
// episode-local ID (reconstructed reference streams carry no identity).
func TestIntentRoundTripPreservesMatcherFields(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		audio := drawIntentStream(t, "audio")
		var sub *Stream
		if rapid.Bool().Draw(t, "has_sub") {
			sub = drawIntentStream(t, "sub")
		}
		observedAt := int64(rapid.IntRange(0, 2_000_000_000).Draw(t, "observed_at"))

		intent := NewIntent(audio, sub, observedAt)

		// Persist and reload as profiles.json would.
		raw, err := json.Marshal(intent)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var loaded Intent
		if err := json.Unmarshal(raw, &loaded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		gotAudio, gotSub := loaded.RefStreams()
		if gotAudio == nil || !matcherFieldsEqual(gotAudio, audio) {
			t.Fatalf("audio round-trip mismatch:\n got %+v\nwant %+v", gotAudio, audio)
		}
		if gotAudio.ID != 0 || gotAudio.Selected || gotAudio.StreamType != 0 {
			t.Fatalf("reconstructed audio carries episode-local identity: %+v", gotAudio)
		}
		if sub == nil {
			if gotSub != nil {
				t.Fatalf("nil subtitle became %+v after round-trip", gotSub)
			}
		} else if gotSub == nil || !matcherFieldsEqual(gotSub, sub) {
			t.Fatalf("subtitle round-trip mismatch:\n got %+v\nwant %+v", gotSub, sub)
		}
		if loaded.ObservedAt != observedAt {
			t.Fatalf("ObservedAt = %d, want %d", loaded.ObservedAt, observedAt)
		}
	})
}

// TestIntentCloneIsolation pins Clone's deep copy: mutating a clone's
// subtitle never leaks into the original.
func TestIntentCloneIsolation(t *testing.T) {
	t.Parallel()
	orig := NewIntent(
		&Stream{LanguageCode: "jpn"},
		&Stream{LanguageCode: "eng"},
		42,
	)
	cl := orig.Clone()
	cl.Subtitle.LanguageCode = "MUTATED"
	if orig.Subtitle.LanguageCode != "eng" {
		t.Errorf("mutating a clone leaked into the original: %q", orig.Subtitle.LanguageCode)
	}
}

// TestIntentReconstructedRefDrivesMatchers is the integration sanity: a
// reconstructed reference stream drives MatchAudio / MatchSubtitle exactly
// like the live stream it was projected from (same winner on a candidate
// set exercising language, codec, and forced-flag discrimination).
func TestIntentReconstructedRefDrivesMatchers(t *testing.T) {
	t.Parallel()
	liveAudio := &Stream{
		ID: 7, StreamType: StreamTypeAudio, Selected: true,
		LanguageCode: "jpn", Codec: "eac3", Channels: 6,
	}
	liveSub := &Stream{
		ID: 8, StreamType: StreamTypeSubtitle, Selected: true,
		LanguageCode: "eng", Codec: "ass", Forced: false,
	}

	audioCandidates := []*Stream{
		{ID: 20, LanguageCode: "eng", Codec: "eac3"},
		{ID: 21, LanguageCode: "jpn", Codec: "aac"},
		{ID: 22, LanguageCode: "jpn", Codec: "eac3"}, // codec match → wins
	}
	subCandidates := []*Stream{
		{ID: 30, LanguageCode: "eng", Codec: "srt", Forced: true},
		{ID: 31, LanguageCode: "eng", Codec: "ass", Forced: false}, // forced+codec match → wins
	}

	intent := NewIntent(liveAudio, liveSub, 1)
	refAudio, refSub := intent.RefStreams()

	liveAudioWinner := MatchAudio(liveAudio, audioCandidates)
	intentAudioWinner := MatchAudio(refAudio, audioCandidates)
	if liveAudioWinner == nil || intentAudioWinner == nil || liveAudioWinner.ID != intentAudioWinner.ID {
		t.Errorf("audio winner differs: live=%v intent=%v", liveAudioWinner, intentAudioWinner)
	}

	liveSubWinner := MatchSubtitle(liveSub, liveAudio, subCandidates)
	intentSubWinner := MatchSubtitle(refSub, refAudio, subCandidates)
	if liveSubWinner == nil || intentSubWinner == nil || liveSubWinner.ID != intentSubWinner.ID {
		t.Errorf("subtitle winner differs: live=%v intent=%v", liveSubWinner, intentSubWinner)
	}
}
