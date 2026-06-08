package ignore

import (
	"context"
	"errors"
	"testing"

	"github.com/cplieger/plex-language-sync/internal/api"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
)

func TestPolicyIgnoreLibrary(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name      string
		title     string
		libraries []string
		want      bool
	}{
		{name: "match", libraries: []string{"Music", "Photos"}, title: "Music", want: true},
		{name: "no match", libraries: []string{"Music"}, title: "TV Shows", want: false},
		{name: "empty libraries", libraries: nil, title: "Music", want: false},
		{name: "case-sensitive miss", libraries: []string{"Music"}, title: "music", want: false},
		{name: "empty title", libraries: []string{"Music"}, title: "", want: false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := NewPolicy(tc.libraries, nil)
			if got := p.IgnoreLibrary(tc.title); got != tc.want {
				t.Errorf("IgnoreLibrary(%q) = %v, want %v", tc.title, got, tc.want)
			}
		})
	}
}

func TestPolicyIgnoreShowLabels(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name   string
		ignore []string
		labels []streams.Label
		want   bool
	}{
		{"match first", []string{"SKIP"}, []streams.Label{{Tag: "SKIP"}, {Tag: "OTHER"}}, true},
		{"match later", []string{"SKIP"}, []streams.Label{{Tag: "OTHER"}, {Tag: "SKIP"}}, true},
		{"no match", []string{"SKIP"}, []streams.Label{{Tag: "OTHER"}}, false},
		{"nil labels", []string{"SKIP"}, nil, false},
		{"empty ignore", nil, []streams.Label{{Tag: "SKIP"}}, false},
		{"both empty", nil, nil, false},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := NewPolicy(nil, tc.ignore)
			if got := p.IgnoreShowLabels(tc.labels); got != tc.want {
				t.Errorf("IgnoreShowLabels(%+v) = %v, want %v", tc.labels, got, tc.want)
			}
		})
	}
}

// stubReader is a minimal api.PlexReader implementation exposing just
// ShowMetadata (the only method ShouldSkipEpisode calls). All other
// methods panic — if a test hits them the signature is wrong.
type stubReader struct {
	show *plex.Show
	err  error
}

func (r *stubReader) Episode(context.Context, plex.RatingKey) (*streams.Episode, error) {
	panic("Episode: not used")
}

func (r *stubReader) ShowEpisodes(context.Context, plex.RatingKey) ([]streams.Episode, error) {
	panic("ShowEpisodes: not used")
}

func (r *stubReader) SeasonEpisodes(context.Context, plex.RatingKey) ([]streams.Episode, error) {
	panic("SeasonEpisodes: not used")
}

func (r *stubReader) ShowMetadata(_ context.Context, _ plex.RatingKey) (*plex.Show, error) {
	return r.show, r.err
}

func (r *stubReader) RecentlyAdded(context.Context, plex.RatingKey, int64) ([]streams.Episode, error) {
	panic("RecentlyAdded: not used")
}

func (r *stubReader) History(context.Context, int64) ([]plex.HistoryItem, error) {
	panic("History: not used")
}

func (r *stubReader) ShowSections(context.Context) ([]plex.Section, error) {
	panic("ShowSections: not used")
}

func (r *stubReader) UserFromSession(context.Context, string) (string, string, error) {
	panic("UserFromSession: not used")
}

func TestPolicyShouldSkipEpisode(t *testing.T) {
	t.Parallel()
	tests := []struct {
		ref       *streams.Episode
		reader    *stubReader
		name      string
		libraries []string
		labels    []string
		want      bool
	}{
		{
			name: "nil ref",
			ref:  nil,
			want: false,
		},
		{
			name:      "library match",
			libraries: []string{"Music"},
			ref:       &streams.Episode{LibraryTitle: "Music", GrandparentRatingKey: "42"},
			reader:    &stubReader{show: &plex.Show{}},
			want:      true,
		},
		{
			name:   "label match",
			labels: []string{"SKIP"},
			ref:    &streams.Episode{LibraryTitle: "TV", GrandparentRatingKey: "42"},
			reader: &stubReader{show: &plex.Show{Label: []streams.Label{{Tag: "SKIP"}}}},
			want:   true,
		},
		{
			name:   "no match",
			labels: []string{"SKIP"},
			ref:    &streams.Episode{LibraryTitle: "TV", GrandparentRatingKey: "42"},
			reader: &stubReader{show: &plex.Show{Label: []streams.Label{{Tag: "OTHER"}}}},
			want:   false,
		},
		{
			name:   "ShowMetadata error returns false",
			labels: []string{"SKIP"},
			ref:    &streams.Episode{LibraryTitle: "TV", GrandparentRatingKey: "42"},
			reader: &stubReader{err: errors.New("boom")},
			want:   false,
		},
		{
			name:   "empty grandparent short-circuits ShowMetadata",
			labels: []string{"SKIP"},
			ref:    &streams.Episode{LibraryTitle: "TV", GrandparentRatingKey: ""},
			reader: nil, // never called
			want:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			p := NewPolicy(tc.libraries, tc.labels)
			var rdr api.PlexReader
			if tc.reader != nil {
				rdr = tc.reader
			}
			if got := p.ShouldSkipEpisode(context.Background(), rdr, tc.ref); got != tc.want {
				t.Errorf("ShouldSkipEpisode = %v, want %v", got, tc.want)
			}
		})
	}
}

// stubReader must satisfy api.PlexReader at compile time.
var _ api.PlexReader = (*stubReader)(nil)

func TestPolicyConstructorDefensiveCopy(t *testing.T) {
	t.Parallel()
	libs := []string{"Music"}
	labs := []string{"SKIP"}
	p := NewPolicy(libs, labs)

	libs[0] = "Photos"
	labs[0] = "MUTATED"

	if p.IgnoreLibrary("Photos") {
		t.Error("NewPolicy did not defensive-copy Libraries")
	}
	if !p.IgnoreLibrary("Music") {
		t.Error("NewPolicy Libraries contents corrupted")
	}
	if p.IgnoreShowLabels([]streams.Label{{Tag: "MUTATED"}}) {
		t.Error("NewPolicy did not defensive-copy Labels")
	}
	if !p.IgnoreShowLabels([]streams.Label{{Tag: "SKIP"}}) {
		t.Error("NewPolicy Labels contents corrupted")
	}
}
