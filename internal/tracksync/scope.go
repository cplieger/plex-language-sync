package tracksync

import (
	"strconv"

	"github.com/cplieger/plex-language-sync/internal/streams"
)

// ScopeKey names the episodes one recorded choice covers: a whole show, or
// one season of it. Its String is the key the choice is stored under.
type ScopeKey struct {
	Show   string
	Season int // showSeason for a whole-show key
}

// showSeason is the Season of a whole-show key. It cannot be 0, which is
// Plex's specials season.
const showSeason = -1

// String is the bare show rating key for a whole-show key, the key every
// show-level intent is stored under, and "<show>/s<season>" for a season key.
func (k ScopeKey) String() string {
	if k.Season == showSeason {
		return k.Show
	}
	return k.Show + "/s" + strconv.Itoa(k.Season)
}

// covers reports whether a record stored under k may govern ep. Coverage
// comes from the key alone, so changing UPDATE_LEVEL never changes what an
// existing record covers.
func (k ScopeKey) covers(ep *streams.Episode) bool {
	show, season := coveringKeys(ep)
	return k == show || k == season
}

// scopeKeyFor is the key a new choice played on ep is recorded under, and
// the only reader of UPDATE_LEVEL.
func scopeKeyFor(level string, ep *streams.Episode) ScopeKey {
	show, season := coveringKeys(ep)
	if level == LevelSeason {
		return season
	}
	return show
}

// coveringKeys are the only two keys whose records can govern ep.
func coveringKeys(ep *streams.Episode) (show, season ScopeKey) {
	return ScopeKey{Show: ep.GrandparentRatingKey, Season: showSeason},
		ScopeKey{Show: ep.GrandparentRatingKey, Season: ep.SeasonNum()}
}

// coversKey is covers for a stored key string. It compares whole strings, so
// "10011" or "1001/sx" never passes for show 1001.
func coversKey(ep *streams.Episode) func(key string) bool {
	show, season := coveringKeys(ep)
	showKey, seasonKey := show.String(), season.String()
	return func(key string) bool { return key == showKey || key == seasonKey }
}

// InScope reports whether a choice recorded under key with anchor governs
// ep: key covers ep and, under StrategyNext, ep comes strictly after the
// anchor's (season, index). A zero anchor keeps every episode key covers.
func InScope(key ScopeKey, strategy string, anchor streams.Anchor, ep *streams.Episode) bool {
	if !key.covers(ep) {
		return false
	}
	if strategy != StrategyNext || anchor.IsZero() {
		return true
	}
	season := ep.SeasonNum()
	return season > anchor.Season || (season == anchor.Season && ep.Num() > anchor.Index)
}

// Scope returns the episodes InScope keeps, in list order.
func Scope(key ScopeKey, strategy string, anchor streams.Anchor, episodes []streams.Episode) []streams.Episode {
	var out []streams.Episode
	for i := range episodes {
		if InScope(key, strategy, anchor, &episodes[i]) {
			out = append(out, episodes[i])
		}
	}
	return out
}
