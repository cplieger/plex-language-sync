package streams

import (
	"encoding/json"
	"testing"

	"github.com/cplieger/langtag/v3"
	"github.com/cplieger/plexapi/v3"
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
		ID:         plexapi.FlexInt(rapid.IntRange(1, 999).Draw(t, label+"_id")),
		StreamType: plexapi.StreamTypeAudio,
		Selected:   true,
	}
}

// matcherFieldsEqual compares the fields the matchers/scorers consume on a
// reference stream (see intentStream's doc).
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

		intent := NewIntent(Pair{Audio: audio, Subtitle: sub}, Anchor{}, observedAt)

		// Persist and reload as profiles.json would.
		raw, err := json.Marshal(intent)
		if err != nil {
			t.Fatalf("marshal: %v", err)
		}
		var loaded Intent
		if err := json.Unmarshal(raw, &loaded); err != nil {
			t.Fatalf("unmarshal: %v", err)
		}

		got := loaded.RefStreams()
		gotAudio, gotSub := got.Audio, got.Subtitle
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
	orig := NewIntent(Pair{
		Audio:    &Stream{LanguageCode: "jpn"},
		Subtitle: &Stream{LanguageCode: "eng"},
	}, Anchor{}, 42)
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
		ID: 7, StreamType: plexapi.StreamTypeAudio, Selected: true,
		LanguageCode: "jpn", Codec: "eac3", Channels: 6,
	}
	liveSub := &Stream{
		ID: 8, StreamType: plexapi.StreamTypeSubtitle, Selected: true,
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

	intent := NewIntent(Pair{Audio: liveAudio, Subtitle: liveSub}, Anchor{}, 1)
	ref := intent.RefStreams()

	liveAudioWinner := MatchAudio(liveAudio, audioCandidates)
	intentAudioWinner := MatchAudio(ref.Audio, audioCandidates)
	if liveAudioWinner == nil || intentAudioWinner == nil || liveAudioWinner.ID != intentAudioWinner.ID {
		t.Errorf("audio winner differs: live=%v intent=%v", liveAudioWinner, intentAudioWinner)
	}

	liveSubWinner := MatchSubtitle(liveSub, subCandidates, langtag.TierIdentical)
	intentSubWinner := MatchSubtitle(ref.Subtitle, subCandidates, langtag.TierIdentical)
	if liveSubWinner == nil || intentSubWinner == nil || liveSubWinner.ID != intentSubWinner.ID {
		t.Errorf("subtitle winner differs: live=%v intent=%v", liveSubWinner, intentSubWinner)
	}
}

// TestIntentDecodeWithoutAnchor pins that an intent written before the
// anchor field loads with a zero anchor and every other field intact.
func TestIntentDecodeWithoutAnchor(t *testing.T) {
	t.Parallel()
	raw := `{"subtitle":{"languageCode":"eng","languageTag":"en","forced":true},` +
		`"audio":{"languageCode":"jpn","codec":"aac","channels":2},"observed_at":1700000000}`
	var in Intent
	if err := json.Unmarshal([]byte(raw), &in); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if !in.Anchor.IsZero() || in.Anchor != (Anchor{}) {
		t.Errorf("Anchor = %+v, want zero", in.Anchor)
	}
	if in.ObservedAt != 1700000000 {
		t.Errorf("ObservedAt = %d, want 1700000000", in.ObservedAt)
	}
	if in.Audio.LanguageCode != "jpn" || in.Audio.Codec != "aac" || in.Audio.Channels != 2 {
		t.Errorf("Audio = %+v", in.Audio)
	}
	if in.Subtitle == nil || in.Subtitle.LanguageCode != "eng" || in.Subtitle.LanguageTag != "en" || !in.Subtitle.Forced {
		t.Errorf("Subtitle = %+v", in.Subtitle)
	}
}

// TestIntentAnchorRoundTrip pins the anchor's JSON shape: an anchored
// intent decodes equal, a zero subtitle id and a zero anchor are omitted.
func TestIntentAnchorRoundTrip(t *testing.T) {
	t.Parallel()
	anchor := Anchor{RatingKey: "1001", Season: 1, Index: 3, AudioID: 9001, SubtitleID: 9002}
	orig := NewIntent(Pair{Audio: &Stream{LanguageCode: "jpn"}, Subtitle: &Stream{LanguageCode: "eng"}}, anchor, 1700000000)
	raw, err := json.Marshal(orig)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	var obj map[string]json.RawMessage
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal map: %v", err)
	}
	const wantAnchor = `{"ratingKey":"1001","season":1,"index":3,"audioID":9001,"subtitleID":9002}`
	if string(obj["anchor"]) != wantAnchor {
		t.Errorf("anchor JSON = %s, want %s", obj["anchor"], wantAnchor)
	}
	var loaded Intent
	if err := json.Unmarshal(raw, &loaded); err != nil {
		t.Fatalf("unmarshal: %v", err)
	}
	if loaded.Anchor != anchor || loaded.Anchor.IsZero() {
		t.Errorf("Anchor = %+v, want %+v", loaded.Anchor, anchor)
	}
	if cl := orig.Clone(); cl.Anchor != anchor {
		t.Errorf("Clone Anchor = %+v, want %+v", cl.Anchor, anchor)
	}

	noSub := NewIntent(Pair{Audio: &Stream{LanguageCode: "jpn"}}, Anchor{RatingKey: "1001", AudioID: 9001}, 1)
	raw, err = json.Marshal(noSub)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal map: %v", err)
	}
	if string(obj["anchor"]) != `{"ratingKey":"1001","season":0,"index":0,"audioID":9001}` {
		t.Errorf("anchor without subtitle JSON = %s", obj["anchor"])
	}

	zero := NewIntent(Pair{Audio: &Stream{LanguageCode: "jpn"}}, Anchor{}, 1)
	raw, err = json.Marshal(zero)
	if err != nil {
		t.Fatalf("marshal: %v", err)
	}
	clear(obj)
	if err := json.Unmarshal(raw, &obj); err != nil {
		t.Fatalf("unmarshal map: %v", err)
	}
	if _, ok := obj["anchor"]; ok {
		t.Errorf("zero anchor was marshalled: %s", raw)
	}
}

// TestAnchorOf pins the anchor taken from an observed selection.
func TestAnchorOf(t *testing.T) {
	t.Parallel()
	ep := &Episode{RatingKey: "1001", ParentIndex: 2, Index: 5}
	audio := &Stream{ID: 9001, StreamType: plexapi.StreamTypeAudio}
	sub := &Stream{ID: 9002, StreamType: plexapi.StreamTypeSubtitle}
	if got, want := AnchorOf(ep, Pair{Audio: audio, Subtitle: sub}),
		(Anchor{RatingKey: "1001", Season: 2, Index: 5, AudioID: 9001, SubtitleID: 9002}); got != want {
		t.Errorf("AnchorOf = %+v, want %+v", got, want)
	}
	if got, want := AnchorOf(ep, Pair{Audio: audio}),
		(Anchor{RatingKey: "1001", Season: 2, Index: 5, AudioID: 9001}); got != want {
		t.Errorf("AnchorOf without subtitle = %+v, want %+v", got, want)
	}
}
