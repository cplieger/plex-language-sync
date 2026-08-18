package ignore

import (
	"context"
	"errors"
	"testing"

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
			p := New(Config{Libraries: tc.libraries})
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
			p := New(Config{Labels: tc.ignore})
			if got := p.IgnoreShowLabels(tc.labels); got != tc.want {
				t.Errorf("IgnoreShowLabels(%+v) = %v, want %v", tc.labels, got, tc.want)
			}
		})
	}
}

// stubReader implements MetadataReader, the one-method interface
// ShouldSkipEpisode needs. It used to implement all eight methods of a shared
// PlexReader interface and panic in seven of them; the narrow interface makes
// those seven unwritable rather than merely unreachable.
type stubReader struct {
	show *plex.Show
	err  error
}

func (r *stubReader) ShowMetadata(_ context.Context, _ plex.RatingKey) (*plex.Show, error) {
	return r.show, r.err
}

// stubReader must satisfy MetadataReader at compile time.
var _ MetadataReader = (*stubReader)(nil)

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
		{
			// A non-empty grandparent with a nil reader must still skip
			// the metadata fetch and report "do not skip" — the nil-reader
			// guard, not just the empty-grandparent guard, has to hold.
			name:   "nil reader with grandparent returns false",
			labels: []string{"SKIP"},
			ref:    &streams.Episode{LibraryTitle: "TV", GrandparentRatingKey: "42"},
			reader: nil, // ShouldSkipEpisode must not dereference it
			want:   false,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			cfg := Config{Libraries: tc.libraries, Labels: tc.labels}
			if tc.reader != nil {
				cfg.Reader = tc.reader
			}
			p := New(cfg)
			if got := p.ShouldSkipEpisode(t.Context(), tc.ref); got != tc.want {
				t.Errorf("ShouldSkipEpisode = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestPolicyConstructorDefensiveCopy(t *testing.T) {
	t.Parallel()
	libs := []string{"Music"}
	labs := []string{"SKIP"}
	p := New(Config{Libraries: libs, Labels: labs})

	libs[0] = "Photos"
	labs[0] = "MUTATED"

	if p.IgnoreLibrary("Photos") {
		t.Error("New did not defensive-copy Libraries")
	}
	if !p.IgnoreLibrary("Music") {
		t.Error("New Libraries contents corrupted")
	}
	if p.IgnoreShowLabels([]streams.Label{{Tag: "MUTATED"}}) {
		t.Error("New did not defensive-copy Labels")
	}
	if !p.IgnoreShowLabels([]streams.Label{{Tag: "SKIP"}}) {
		t.Error("New Labels contents corrupted")
	}
}

// TestPolicyShouldSkipEpisodeEmptyGrandparentSkipsLabelFetch isolates the
// empty-grandparent half of the "GrandparentRatingKey == \"\" || reader == nil"
// guard from the nil-reader half. ShouldSkipEpisode is 100% statement-covered,
// but no existing case pairs an empty grandparent with a non-nil reader, so a
// regression dropping the GrandparentRatingKey == "" check survives. Here the
// reader is non-nil AND its show carries a matching ignore label: the empty
// grandparent must short-circuit to false before that label is ever fetched;
// dropping the check would fetch the show and return true.
func TestPolicyShouldSkipEpisodeEmptyGrandparentSkipsLabelFetch(t *testing.T) {
	t.Parallel()
	reader := &stubReader{show: &plex.Show{Label: []streams.Label{{Tag: "SKIP"}}}}
	p := New(Config{Reader: reader, Labels: []string{"SKIP"}})
	ref := &streams.Episode{LibraryTitle: "TV", GrandparentRatingKey: ""}
	if p.ShouldSkipEpisode(t.Context(), ref) {
		t.Error("ShouldSkipEpisode with empty GrandparentRatingKey = true, want false " +
			"(the empty-grandparent guard must short-circuit before the show-label fetch)")
	}
}
