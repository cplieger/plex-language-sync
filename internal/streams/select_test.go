package streams

import (
	"testing"

	"github.com/cplieger/plexapi/v3"
)

func TestSelectedStreams(t *testing.T) {
	ep := &Episode{
		Media: []Media{{
			Part: []Part{{
				Stream: []Stream{
					{ID: 1, StreamType: plexapi.StreamTypeVideo, Selected: true},
					{ID: 2, StreamType: plexapi.StreamTypeAudio, Selected: false, LanguageCode: "eng"},
					{ID: 3, StreamType: plexapi.StreamTypeAudio, Selected: true, LanguageCode: "jpn"},
					{ID: 4, StreamType: plexapi.StreamTypeSubtitle, Selected: true, LanguageCode: "eng"},
					{ID: 5, StreamType: plexapi.StreamTypeSubtitle, Selected: false, LanguageCode: "jpn"},
				},
			}},
		}},
	}

	sel := Selected(ep)
	audio, sub := sel.Audio, sel.Subtitle
	if audio == nil || audio.ID != 3 {
		t.Errorf("expected audio stream ID=3, got %v", audio)
	}
	if sub == nil || sub.ID != 4 {
		t.Errorf("expected subtitle stream ID=4, got %v", sub)
	}
}

func TestSelectedStreamsEmpty(t *testing.T) {
	ep := &Episode{}
	sel := Selected(ep)
	audio, sub := sel.Audio, sel.Subtitle
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
			{ID: 1, StreamType: plexapi.StreamTypeVideo},
			{ID: 2, StreamType: plexapi.StreamTypeAudio, LanguageCode: "eng"},
			{ID: 3, StreamType: plexapi.StreamTypeSubtitle, LanguageCode: "eng"},
			{ID: 4, StreamType: plexapi.StreamTypeAudio, LanguageCode: "jpn"},
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
			{ID: 1, StreamType: plexapi.StreamTypeAudio},
			{ID: 2, StreamType: plexapi.StreamTypeSubtitle, LanguageCode: "eng"},
			{ID: 3, StreamType: plexapi.StreamTypeSubtitle, LanguageCode: "jpn"},
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
					{ID: 1, StreamType: plexapi.StreamTypeAudio, Selected: false},
					{ID: 2, StreamType: plexapi.StreamTypeSubtitle, Selected: false},
				},
			}},
		}},
	}
	sel := Selected(ep)
	audio, sub := sel.Audio, sel.Subtitle
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
				{ID: 1, StreamType: plexapi.StreamTypeAudio, Selected: true, LanguageCode: "eng"},
			}}}},
			{Part: []Part{{Stream: []Stream{
				{ID: 2, StreamType: plexapi.StreamTypeAudio, Selected: true, LanguageCode: "jpn"},
			}}}},
		},
	}
	audio := Selected(ep).Audio
	if audio == nil || audio.ID != 1 {
		t.Errorf("Selected should use first media, got audio ID=%v", audio)
	}
}

func TestDefaultAudio(t *testing.T) {
	audio := func(id int, def bool) Stream {
		s := Stream{}
		s.ID, s.StreamType, s.Default = plexapi.FlexInt(id), plexapi.StreamTypeAudio, def
		return s
	}
	sub := func(id int, def bool) Stream {
		s := Stream{}
		s.ID, s.StreamType, s.Default = plexapi.FlexInt(id), plexapi.StreamTypeSubtitle, def
		return s
	}
	tests := []struct {
		name string
		ep   *Episode
		want int // 0 = nil
	}{
		{
			name: "default audio after a non-default one",
			ep: &Episode{Media: []Media{{Part: []Part{{Stream: []Stream{
				sub(9003, true), audio(9001, false), audio(9002, true),
			}}}}}},
			want: 9002,
		},
		{
			name: "no audio marked default takes the first audio",
			ep: &Episode{Media: []Media{{Part: []Part{{Stream: []Stream{
				sub(9003, true), audio(9001, false), audio(9002, false),
			}}}}}},
			want: 9001,
		},
		{
			name: "a default subtitle is not audio",
			ep: &Episode{Media: []Media{{Part: []Part{{Stream: []Stream{
				sub(9003, true),
			}}}}}},
			want: 0,
		},
		{
			name: "no media",
			ep:   &Episode{},
			want: 0,
		},
		{
			name: "nil episode",
			ep:   nil,
			want: 0,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := DefaultAudio(tt.ep)
			switch {
			case tt.want == 0 && got != nil:
				t.Errorf("DefaultAudio() = stream %d, want nil", got.ID)
			case tt.want != 0 && (got == nil || int(got.ID) != tt.want):
				t.Errorf("DefaultAudio() = %v, want stream %d", got, tt.want)
			}
		})
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
