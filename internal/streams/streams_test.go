package streams

import (
	"testing"

	"pgregory.net/rapid"
)

func TestStreamDesc(t *testing.T) {
	if got := Desc(nil); got != "none" {
		t.Errorf("Desc(nil) = %q, want %q", got, "none")
	}
	s := &Stream{ID: 1, ExtendedDisplayTitle: "English (EAC3 5.1)"}
	if got := Desc(s); got != "English (EAC3 5.1)" {
		t.Errorf("Desc() = %q, want %q", got, "English (EAC3 5.1)")
	}
}

func TestContainsDescriptive(t *testing.T) {
	tests := []struct {
		title string
		want  bool
	}{
		{"english", false},
		{"english (commentary)", true},
		{"audio description", true},
		{"descriptive audio", true},
		{"", false},
	}
	for _, tt := range tests {
		if got := ContainsDescriptive(tt.title); got != tt.want {
			t.Errorf("ContainsDescriptive(%q) = %v, want %v", tt.title, got, tt.want)
		}
	}
}

func TestEpisodeMethods(t *testing.T) {
	ep := &Episode{
		ParentIndex:      2,
		Index:            5,
		GrandparentTitle: "Breaking Bad",
	}
	if got := ep.SeasonNum(); got != 2 {
		t.Errorf("seasonNum() = %d, want 2", got)
	}
	if got := ep.EpisodeNum(); got != 5 {
		t.Errorf("episodeNum() = %d, want 5", got)
	}
	if got := ep.ShortName(); got != "'Breaking Bad' (S02E05)" {
		t.Errorf("shortName() = %q", got)
	}
}

func TestEpisodeMethodsZero(t *testing.T) {
	// Zero-valued FlexInt matches the semantics of the previous
	// json.Number-backed fields when the JSON field was absent or
	// null: SeasonNum and EpisodeNum both return 0. Malformed
	// on-wire inputs (e.g. "abc") now fail FlexInt.UnmarshalJSON
	// at decode time rather than silently falling through to 0;
	// that path is covered in flex_test.go.
	ep := &Episode{}
	if got := ep.SeasonNum(); got != 0 {
		t.Errorf("seasonNum() = %d, want 0", got)
	}
	if got := ep.EpisodeNum(); got != 0 {
		t.Errorf("episodeNum() = %d, want 0", got)
	}
}

// --- Tests: cache operations ---

func TestStreamIsAudioIsSubtitle(t *testing.T) {
	audio := Stream{StreamType: StreamTypeAudio}
	sub := Stream{StreamType: StreamTypeSubtitle}
	video := Stream{StreamType: StreamTypeVideo}

	if !audio.IsAudio() {
		t.Error("expected isAudio() true for StreamTypeAudio")
	}
	if audio.IsSubtitle() {
		t.Error("expected isSubtitle() false for StreamTypeAudio")
	}
	if !sub.IsSubtitle() {
		t.Error("expected isSubtitle() true for StreamTypeSubtitle")
	}
	if video.IsAudio() || video.IsSubtitle() {
		t.Error("video stream should not be audio or subtitle")
	}
}

// --- Tests: titleForMatch ---

func TestTitleForMatch(t *testing.T) {
	tests := []struct {
		name string
		want string
		s    Stream
	}{
		{name: "extended first", s: Stream{ExtendedDisplayTitle: "ext", DisplayTitle: "disp", Title: "t"}, want: "ext"},
		{name: "display second", s: Stream{DisplayTitle: "disp", Title: "t"}, want: "disp"},
		{name: "title last", s: Stream{Title: "t"}, want: "t"},
		{name: "empty", s: Stream{}, want: ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := tt.s.TitleForMatch(); got != tt.want {
				t.Errorf("titleForMatch() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- Tests: shouldIgnoreLibrary ---

func TestStreamDescAllBranches(t *testing.T) {
	tests := []struct {
		name string
		s    *Stream
		want string
	}{
		{"nil", nil, "none"},
		{"extended title", &Stream{ExtendedDisplayTitle: "English (EAC3 5.1)"}, "English (EAC3 5.1)"},
		{"display title", &Stream{DisplayTitle: "English"}, "English"},
		{"title only", &Stream{Title: "Eng"}, "Eng"},
		{"fallback to ID", &Stream{ID: 42}, "stream-42"},
		{"ID zero", &Stream{}, "stream-0"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := Desc(tt.s)
			if got != tt.want {
				t.Errorf("Desc() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- Tests: userManager.allUsers ---

func TestContainsDescriptiveAllTerms(t *testing.T) {
	terms := []string{
		"commentary", "description", "descriptive",
		"narration", "narrative", "described",
	}
	for _, term := range terms {
		if !ContainsDescriptive(term) {
			t.Errorf("ContainsDescriptive(%q) should be true", term)
		}
	}
	if ContainsDescriptive("normal audio track") {
		t.Error("normal track should not be descriptive")
	}
}

// --- Tests: Episode.ShortName formatting ---

func TestEpisodeShortNameFormatting(t *testing.T) {
	tests := []struct {
		name string
		want string
		ep   Episode
	}{
		{
			name: "single digit season and episode",
			ep:   Episode{ParentIndex: 1, Index: 3, GrandparentTitle: "Show"},
			want: "'Show' (S01E03)",
		},
		{
			name: "double digit",
			ep:   Episode{ParentIndex: 12, Index: 24, GrandparentTitle: "Big Show"},
			want: "'Big Show' (S12E24)",
		},
		{
			name: "zero (absent or null JSON)",
			ep:   Episode{GrandparentTitle: "Bad"},
			want: "'Bad' (S00E00)",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.ep.ShortName()
			if got != tt.want {
				t.Errorf("shortName() = %q, want %q", got, tt.want)
			}
		})
	}
}

// --- Tests: Audio / Subtitle nil guards ---

func TestContainsDescriptiveNeverPanics(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		title := rapid.String().Draw(t, "title")
		ContainsDescriptive(title)
	})
}

func TestStreamDescPriorityOrder(t *testing.T) {
	// Verify the priority: ExtendedDisplayTitle > DisplayTitle > Title > ID fallback.
	s := &Stream{
		ID:                   99,
		Title:                "Title",
		DisplayTitle:         "Display",
		ExtendedDisplayTitle: "Extended",
	}
	if got := Desc(s); got != "Extended" {
		t.Errorf("Desc with all fields = %q, want Extended", got)
	}

	s.ExtendedDisplayTitle = ""
	if got := Desc(s); got != "Display" {
		t.Errorf("Desc without extended = %q, want Display", got)
	}

	s.DisplayTitle = ""
	if got := Desc(s); got != "Title" {
		t.Errorf("Desc without display = %q, want Title", got)
	}

	s.Title = ""
	if got := Desc(s); got != "stream-99" {
		t.Errorf("Desc with only ID = %q, want stream-99", got)
	}
}

func TestStreamID(t *testing.T) {
	t.Parallel()
	if got := ID(nil); got != 0 {
		t.Errorf("ID(nil) = %d, want 0", got)
	}
	if got := ID(&Stream{ID: 42}); got != 42 {
		t.Errorf("ID({ID:42}) = %d, want 42", got)
	}
}

// --- Tests: findReferenceEpisode (q1) ---
