package tracksync

import (
	"slices"
	"strconv"
	"testing"

	"github.com/cplieger/plex-language-sync/internal/streams"
)

// scopeEp is an episode of show 1001 at (season, index), keyed "s<season>e<index>".
func scopeEp(season, index int) streams.Episode {
	return showEp("1001", season, index)
}

func showEp(show string, season, index int) streams.Episode {
	return streams.Episode{
		RatingKey:            "s" + strconv.Itoa(season) + "e" + strconv.Itoa(index),
		GrandparentRatingKey: show,
		ParentIndex:          streams.FlexInt(season),
		Index:                streams.FlexInt(index),
	}
}

func anchorAt(season, index int) streams.Anchor {
	return streams.Anchor{RatingKey: "s" + strconv.Itoa(season) + "e" + strconv.Itoa(index), Season: season, Index: index}
}

func ratingKeys(eps []streams.Episode) []string {
	out := []string{}
	for i := range eps {
		out = append(out, eps[i].RatingKey)
	}
	return out
}

func TestScope(t *testing.T) {
	t.Parallel()
	twoSeasons := []streams.Episode{
		scopeEp(1, 1), scopeEp(1, 2), scopeEp(1, 3), scopeEp(1, 4),
		scopeEp(2, 1), scopeEp(2, 2), scopeEp(2, 3),
	}
	show := scopeKey{Show: "1001", Season: -1}
	season1 := scopeKey{Show: "1001", Season: 1}
	season2 := scopeKey{Show: "1001", Season: 2}
	tests := []struct {
		name     string
		key      scopeKey
		strategy string
		anchor   streams.Anchor
		episodes []streams.Episode
		want     []string
	}{
		{
			name: "show key all anchored keeps both seasons", key: show, strategy: StrategyAll,
			anchor: anchorAt(1, 3), episodes: twoSeasons,
			want: []string{"s1e1", "s1e2", "s1e3", "s1e4", "s2e1", "s2e2", "s2e3"},
		},
		{
			name: "show key next anchored keeps after the anchor across seasons", key: show, strategy: StrategyNext,
			anchor: anchorAt(1, 3), episodes: twoSeasons,
			want: []string{"s1e4", "s2e1", "s2e2", "s2e3"},
		},
		{
			name: "show key next zero anchor keeps the whole show", key: show, strategy: StrategyNext,
			episodes: twoSeasons,
			want:     []string{"s1e1", "s1e2", "s1e3", "s1e4", "s2e1", "s2e2", "s2e3"},
		},
		{
			name: "show key all zero anchor keeps the whole show", key: show, strategy: StrategyAll,
			episodes: twoSeasons,
			want:     []string{"s1e1", "s1e2", "s1e3", "s1e4", "s2e1", "s2e2", "s2e3"},
		},
		{
			name: "season 1 key all keeps its season only", key: season1, strategy: StrategyAll,
			anchor: anchorAt(1, 3), episodes: twoSeasons,
			want: []string{"s1e1", "s1e2", "s1e3", "s1e4"},
		},
		{
			name: "season 1 key next keeps its season after the anchor", key: season1, strategy: StrategyNext,
			anchor: anchorAt(1, 3), episodes: twoSeasons,
			want: []string{"s1e4"},
		},
		{
			name: "season 1 key next zero anchor keeps its season", key: season1, strategy: StrategyNext,
			episodes: twoSeasons,
			want:     []string{"s1e1", "s1e2", "s1e3", "s1e4"},
		},
		{
			name: "season 2 key all keeps its season only", key: season2, strategy: StrategyAll,
			anchor: anchorAt(2, 1), episodes: twoSeasons,
			want: []string{"s2e1", "s2e2", "s2e3"},
		},
		{
			name: "season 2 key next keeps its season after the anchor", key: season2, strategy: StrategyNext,
			anchor: anchorAt(2, 1), episodes: twoSeasons,
			want: []string{"s2e2", "s2e3"},
		},
		{
			name: "season 2 key all zero anchor keeps its season", key: season2, strategy: StrategyAll,
			episodes: twoSeasons,
			want:     []string{"s2e1", "s2e2", "s2e3"},
		},
		{
			name: "next across three seasons keeps strictly later episodes", key: show, strategy: StrategyNext,
			anchor: anchorAt(2, 5),
			episodes: []streams.Episode{
				scopeEp(1, 1), scopeEp(2, 3), scopeEp(2, 5), scopeEp(2, 7), scopeEp(3, 1),
			},
			want: []string{"s2e7", "s3e1"},
		},
		{
			name: "next drops the anchor episode itself", key: show, strategy: StrategyNext,
			anchor: anchorAt(1, 5), episodes: []streams.Episode{scopeEp(1, 5)},
			want: []string{},
		},
		{
			name: "empty list", key: show, strategy: StrategyNext,
			anchor: anchorAt(1, 1),
			want:   []string{},
		},
		{
			name: "an episode of another show is not covered", key: show, strategy: StrategyAll,
			episodes: []streams.Episode{scopeEp(1, 1), showEp("1002", 1, 2)},
			want:     []string{"s1e1"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			got := ratingKeys(scopeOf(tt.key, tt.strategy, tt.anchor, tt.episodes))
			if !slices.Equal(got, tt.want) {
				t.Errorf("scopeOf(%q, %q, %+v) = %v, want %v", tt.key.String(), tt.strategy, tt.anchor, got, tt.want)
			}
		})
	}
}

func TestInScope(t *testing.T) {
	t.Parallel()
	show := scopeKey{Show: "1001", Season: -1}
	season1 := scopeKey{Show: "1001", Season: 1}
	tests := []struct {
		name     string
		key      scopeKey
		strategy string
		anchor   streams.Anchor
		ep       streams.Episode
		want     bool
	}{
		{name: "show key all covers a later season", key: show, strategy: StrategyAll, anchor: anchorAt(1, 3), ep: scopeEp(2, 1), want: true},
		{name: "show key next covers a later season", key: show, strategy: StrategyNext, anchor: anchorAt(1, 3), ep: scopeEp(2, 1), want: true},
		{name: "show key next refuses an earlier episode", key: show, strategy: StrategyNext, anchor: anchorAt(1, 3), ep: scopeEp(1, 2), want: false},
		{name: "show key next refuses the anchor itself", key: show, strategy: StrategyNext, anchor: anchorAt(1, 3), ep: scopeEp(1, 3), want: false},
		{name: "show key next zero anchor covers an early episode", key: show, strategy: StrategyNext, ep: scopeEp(1, 1), want: true},
		{name: "season 1 key refuses season 2 under all", key: season1, strategy: StrategyAll, anchor: anchorAt(1, 3), ep: scopeEp(2, 1), want: false},
		{name: "season 1 key refuses season 2 under next", key: season1, strategy: StrategyNext, anchor: anchorAt(1, 3), ep: scopeEp(2, 1), want: false},
		{name: "season 1 key zero anchor refuses season 2", key: season1, strategy: StrategyNext, ep: scopeEp(2, 1), want: false},
		{name: "season 1 key next covers a later episode of its season", key: season1, strategy: StrategyNext, anchor: anchorAt(1, 3), ep: scopeEp(1, 4), want: true},
		{name: "show key refuses another show", key: show, strategy: StrategyAll, ep: showEp("1002", 1, 1), want: false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			if got := inScope(tt.key, tt.strategy, tt.anchor, &tt.ep); got != tt.want {
				t.Errorf("inScope(%q, %q, %+v, %s) = %v, want %v", tt.key.String(), tt.strategy, tt.anchor, tt.ep.RatingKey, got, tt.want)
			}
		})
	}
}

func TestScopeKeyString(t *testing.T) {
	t.Parallel()
	t.Run("show key", func(t *testing.T) {
		t.Parallel()
		if got := (scopeKey{Show: "1001", Season: -1}).String(); got != "1001" {
			t.Errorf("show key String() = %q, want %q", got, "1001")
		}
	})
	t.Run("season key", func(t *testing.T) {
		t.Parallel()
		if got := (scopeKey{Show: "1001", Season: 2}).String(); got != "1001/s2" {
			t.Errorf("season key String() = %q, want %q", got, "1001/s2")
		}
	})
	t.Run("specials season key", func(t *testing.T) {
		t.Parallel()
		if got := (scopeKey{Show: "1001", Season: 0}).String(); got != "1001/s0" {
			t.Errorf("season 0 key String() = %q, want %q", got, "1001/s0")
		}
	})
	t.Run("show level key is the bare show rating key", func(t *testing.T) {
		t.Parallel()
		ep := scopeEp(2, 1)
		if got := scopeKeyFor(LevelShow, &ep).String(); got != ep.GrandparentRatingKey {
			t.Errorf("scopeKeyFor(show).String() = %q, want the show rating key %q", got, ep.GrandparentRatingKey)
		}
	})
	t.Run("season level key names the played season", func(t *testing.T) {
		t.Parallel()
		ep := scopeEp(2, 1)
		if got, want := scopeKeyFor(LevelSeason, &ep), (scopeKey{Show: "1001", Season: 2}); got != want {
			t.Errorf("scopeKeyFor(season) = %+v, want %+v", got, want)
		}
	})
}

func TestCoversKey(t *testing.T) {
	t.Parallel()
	ep := scopeEp(2, 1)
	covers := coversKey(&ep)
	for _, key := range []string{"1001/s2", "1001"} {
		if !covers(key) {
			t.Errorf("coversKey(S2E1)(%q) = false, want true", key)
		}
	}
	for _, key := range []string{"1001/s1", "10011", "10011/s2", "1001/sx", "1001/s", ""} {
		if covers(key) {
			t.Errorf("coversKey(S2E1)(%q) = true, want false", key)
		}
	}
	show, season := coveringKeys(&ep)
	if want := (scopeKey{Show: "1001", Season: -1}); show != want {
		t.Errorf("coveringKeys show = %+v, want %+v", show, want)
	}
	if want := (scopeKey{Show: "1001", Season: 2}); season != want {
		t.Errorf("coveringKeys season = %+v, want %+v", season, want)
	}
}

func TestKeysOfShow(t *testing.T) {
	t.Parallel()
	of := keysOfShow("1001")
	for key, want := range map[string]bool{"1001": true, "1001/s2": true, "1001/s0": true, "10011": false, "10011/s2": false, "100": false, "": false} {
		if got := of(key); got != want {
			t.Errorf("keysOfShow(1001)(%q) = %v, want %v", key, got, want)
		}
	}
}
