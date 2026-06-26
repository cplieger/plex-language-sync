package streams

import (
	"testing"
)

func TestSelectedStreams(t *testing.T) {
	ep := &Episode{
		Media: []Media{{
			Part: []Part{{
				Stream: []Stream{
					{ID: 1, StreamType: StreamTypeVideo, Selected: true},
					{ID: 2, StreamType: StreamTypeAudio, Selected: false, LanguageCode: "eng"},
					{ID: 3, StreamType: StreamTypeAudio, Selected: true, LanguageCode: "jpn"},
					{ID: 4, StreamType: StreamTypeSubtitle, Selected: true, LanguageCode: "eng"},
					{ID: 5, StreamType: StreamTypeSubtitle, Selected: false, LanguageCode: "jpn"},
				},
			}},
		}},
	}

	audio, sub := Selected(ep)
	if audio == nil || audio.ID != 3 {
		t.Errorf("expected audio stream ID=3, got %v", audio)
	}
	if sub == nil || sub.ID != 4 {
		t.Errorf("expected subtitle stream ID=4, got %v", sub)
	}
}

func TestSelectedStreamsEmpty(t *testing.T) {
	ep := &Episode{}
	audio, sub := Selected(ep)
	if audio != nil || sub != nil {
		t.Error("expected nil streams for empty episode")
	}
}

func TestFirstPartID(t *testing.T) {
	t.Run("returns first part ID", func(t *testing.T) {
		ep := &Episode{
			Media: []Media{{Part: []Part{{ID: 42}}}},
		}
		if got := FirstPartID(ep); got != 42 {
			t.Errorf("FirstPartID() = %d, want 42", got)
		}
	})

	t.Run("returns 0 for empty", func(t *testing.T) {
		if got := FirstPartID(&Episode{}); got != 0 {
			t.Errorf("FirstPartID() = %d, want 0", got)
		}
	})
}

// --- Tests: Audio / Subtitle ---

func TestAudioStreams(t *testing.T) {
	ep := &Episode{
		Media: []Media{{Part: []Part{{Stream: []Stream{
			{ID: 1, StreamType: StreamTypeVideo},
			{ID: 2, StreamType: StreamTypeAudio, LanguageCode: "eng"},
			{ID: 3, StreamType: StreamTypeSubtitle, LanguageCode: "eng"},
			{ID: 4, StreamType: StreamTypeAudio, LanguageCode: "jpn"},
		}}}}},
	}
	got := Audio(ep)
	if len(got) != 2 {
		t.Fatalf("expected 2 audio streams, got %d", len(got))
	}
	if got[0].ID != 2 || got[1].ID != 4 {
		t.Errorf("unexpected IDs: %d, %d", got[0].ID, got[1].ID)
	}
}

func TestSubtitleStreams(t *testing.T) {
	ep := &Episode{
		Media: []Media{{Part: []Part{{Stream: []Stream{
			{ID: 1, StreamType: StreamTypeAudio},
			{ID: 2, StreamType: StreamTypeSubtitle, LanguageCode: "eng"},
			{ID: 3, StreamType: StreamTypeSubtitle, LanguageCode: "jpn"},
		}}}}},
	}
	got := Subtitle(ep)
	if len(got) != 2 {
		t.Fatalf("expected 2 subtitle streams, got %d", len(got))
	}
}

func TestAudioStreamsEmpty(t *testing.T) {
	ep := &Episode{}
	got := Audio(ep)
	if got != nil {
		t.Errorf("expected nil for empty episode, got %d streams", len(got))
	}
}

func TestAudioStreamsEmptyParts(t *testing.T) {
	ep := &Episode{Media: []Media{{}}}
	got := Audio(ep)
	if got != nil {
		t.Errorf("expected nil for empty parts, got %d streams", len(got))
	}
}

func TestSubtitleStreamsEmpty(t *testing.T) {
	ep := &Episode{}
	got := Subtitle(ep)
	if got != nil {
		t.Errorf("expected nil for empty episode, got %d streams", len(got))
	}
}

func TestSubtitleStreamsEmptyParts(t *testing.T) {
	ep := &Episode{Media: []Media{{}}}
	got := Subtitle(ep)
	if got != nil {
		t.Errorf("expected nil for empty parts, got %d streams", len(got))
	}
}

func TestSelectedStreamsNoSelection(t *testing.T) {
	ep := &Episode{
		Media: []Media{{
			Part: []Part{{
				Stream: []Stream{
					{ID: 1, StreamType: StreamTypeAudio, Selected: false},
					{ID: 2, StreamType: StreamTypeSubtitle, Selected: false},
				},
			}},
		}},
	}
	audio, sub := Selected(ep)
	if audio != nil {
		t.Error("expected nil audio when nothing selected")
	}
	if sub != nil {
		t.Error("expected nil subtitle when nothing selected")
	}
}

func TestSelectedStreamsMultipleMedia(t *testing.T) {
	// Selected only looks at first media, first part.
	ep := &Episode{
		Media: []Media{
			{Part: []Part{{Stream: []Stream{
				{ID: 1, StreamType: StreamTypeAudio, Selected: true, LanguageCode: "eng"},
			}}}},
			{Part: []Part{{Stream: []Stream{
				{ID: 2, StreamType: StreamTypeAudio, Selected: true, LanguageCode: "jpn"},
			}}}},
		},
	}
	audio, _ := Selected(ep)
	if audio == nil || audio.ID != 1 {
		t.Errorf("Selected should use first media, got audio ID=%v", audio)
	}
}

func TestFirstPartIDMultipleMedia(t *testing.T) {
	ep := &Episode{
		Media: []Media{
			{Part: []Part{{ID: 100}, {ID: 200}}},
			{Part: []Part{{ID: 300}}},
		},
	}
	if got := FirstPartID(ep); got != 100 {
		t.Errorf("FirstPartID() = %d, want 100 (first media, first part)", got)
	}
}
