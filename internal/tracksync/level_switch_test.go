package tracksync

import (
	"slices"
	"testing"

	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plex-language-sync/internal/testsupport/fakeapi"
)

// A whole-show choice keeps covering every season after UPDATE_LEVEL moves to
// season, until a season gets a newer choice of its own. Each episode is added
// to the library only when the step that must write it runs, so no record of
// an earlier write can explain a write the test asserts.
func TestLevelSwitchShowChoiceCoversEverySeason(t *testing.T) {
	t.Parallel()
	s1e1, s1e2, s1e3 := seedEp(1, 1), seedEp(1, 2), seedEp(1, 3)
	s2 := []streams.Episode{seedEp(2, 1), seedEp(2, 2), seedEp(2, 3), seedEp(2, 4), seedEp(2, 5), seedEp(2, 6)}
	l := newSeedLib(t, allShowSeeding, []string{"alice"}, s1e1, s1e2)
	writesFrom := func(from int, lang string, eps ...*streams.Episode) {
		t.Helper()
		var want []fakeapi.Write
		for _, ep := range eps {
			want = append(want, audioWrite(ep, lang))
		}
		if got := l.writes("alice", from); !slices.Equal(got, want) {
			t.Errorf("writes = %+v, want %+v", got, want)
		}
	}
	mark := func() int { return len(l.clients["alice"].Writes()) }

	l.play(t, "alice", &s1e1, streamOf(&s1e1, kindAudio, "jpn"))
	writesFrom(0, "jpn", &s1e2)
	showChoice, ok := l.cache.IntentFor("alice", obsShow)
	if !ok || showChoice.Anchor.RatingKey != s1e1.RatingKey {
		t.Fatalf("show choice = %+v, %v; want one anchored on S1E1", showChoice, ok)
	}

	for i := range 3 {
		l.add(s2[i])
	}
	l.build(Config{UpdateLevel: LevelSeason, UpdateStrategy: StrategyAll, SeedNewEpisodes: true})
	l.clock = 2000
	from := mark()
	l.reconcile(t, "alice", &s2[0])
	writesFrom(from, "jpn", &s2[0], &s2[1], &s2[2])

	l.add(s2[3])
	from = mark()
	l.seed(t, &s2[3])
	writesFrom(from, "jpn", &s2[3])
	if got, want := l.basis("alice", &s2[3]), (streams.Basis{Version: showChoice.ObservedAt, Source: streams.SourceIntent}); got != want {
		t.Errorf("seed basis = %+v, want the show choice's %+v", got, want)
	}

	l.clock = 3000
	from = mark()
	l.play(t, "alice", &s2[1], streamOf(&s2[1], kindAudio, "fra"))
	writesFrom(from, "fra", &s2[0], &s2[2], &s2[3])
	seasonChoice, ok := l.cache.IntentFor("alice", "1001/s2")
	if !ok || seasonChoice.Anchor.RatingKey != s2[1].RatingKey {
		t.Fatalf("season choice = %+v, %v; want one anchored on S2E2", seasonChoice, ok)
	}
	if got, _ := l.cache.IntentFor("alice", obsShow); got.ObservedAt != showChoice.ObservedAt || got.Audio.LanguageCode != "jpn" {
		t.Errorf("show choice after the season choice = %+v, want it unchanged", got)
	}

	l.add(s2[4])
	l.clock = 4000
	from = mark()
	l.reconcile(t, "alice", &s1e2)
	writesFrom(from, "jpn")

	calls := len(l.clients["alice"].CallNames())
	l.reconcile(t, "alice", &s2[0])
	writesFrom(from, "fra", &s2[4])
	if got, want := l.clients["alice"].CallNames()[calls:], []string{"SeasonEpisodes:302", "Episode:" + s2[4].RatingKey, "SetAudio", "Episode:" + s2[4].RatingKey}; !slices.Equal(got, want) {
		t.Errorf("season replay requests = %v, want %v (only the new episode is read, written and read back; every part with a record is skipped unread)", got, want)
	}

	l.add(s2[5])
	l.add(s1e3)
	from = mark()
	l.seed(t, &s2[5])
	l.seed(t, &s1e3)
	if got, want := l.writes("alice", from), []fakeapi.Write{audioWrite(&s2[5], "fra"), audioWrite(&s1e3, "jpn")}; !slices.Equal(got, want) {
		t.Errorf("seeds = %+v, want season 2 on French and season 1 on Japanese %+v", got, want)
	}

	from = mark()
	l.play(t, "alice", &s2[2], streamOf(&s2[2], kindAudio, "fra"))
	writesFrom(from, "fra")
	if got, _ := l.cache.IntentFor("alice", "1001/s2"); got.ObservedAt != seasonChoice.ObservedAt {
		t.Errorf("season choice after an unchanged play = %+v, want it unchanged", got)
	}
}

// Older versions stored every choice under the show key, and at
// UPDATE_LEVEL=season applied it to the replayed episode's season only. Such a
// record has no anchor, and its replay keeps that season.
func TestLegacyShowRecordAtSeasonLevelKeepsItsSeason(t *testing.T) {
	t.Parallel()
	s1e1, s1e2, s2e1, s2e2 := seedEp(1, 1), seedEp(1, 2), seedEp(2, 1), seedEp(2, 2)
	cfg := Config{UpdateLevel: LevelSeason, UpdateStrategy: StrategyAll, SeedNewEpisodes: true}
	l := newSeedLib(t, cfg, []string{"alice"}, s1e1, s1e2, s2e1, s2e2)
	fra := obsAud(0, "fra", "")
	l.cache.RecordIntent("alice", obsShow, streams.NewIntent(streams.Pair{Audio: &fra}, streams.Anchor{}, 500))

	l.reconcile(t, "alice", &s2e1)
	if got, want := l.writes("alice", 0), []fakeapi.Write{audioWrite(&s2e1, "fra"), audioWrite(&s2e2, "fra")}; !slices.Equal(got, want) {
		t.Errorf("writes = %+v, want season 2 only %+v", got, want)
	}
}

// A whole-show choice outranks a season choice saved earlier, even when the
// season's ObservedAt ran ahead of the clock: two season choices in one second
// stamp the second a second ahead, and a restart can follow within it.
func TestShowChoiceAfterBumpedSeasonChoiceGoverns(t *testing.T) {
	t.Parallel()
	s1e1, s1e2, s2e1, s2e2 := seedEp(1, 1), seedEp(1, 2), seedEp(2, 1), seedEp(2, 2)
	l := newSeedLib(t, Config{UpdateLevel: LevelSeason, UpdateStrategy: StrategyAll, SeedNewEpisodes: true},
		[]string{"alice"}, s1e1, s1e2, s2e1, s2e2)
	l.play(t, "alice", &s2e1, streamOf(&s2e1, kindAudio, "fra"))
	l.play(t, "alice", &s2e1, streamOf(&s2e1, kindAudio, "eng"))

	l.build(allShowSeeding)
	from := len(l.clients["alice"].Writes())
	l.play(t, "alice", &s1e1, streamOf(&s1e1, kindAudio, "fra"))
	if got, want := l.writes("alice", from), []fakeapi.Write{audioWrite(&s1e2, "fra"), audioWrite(&s2e1, "fra"), audioWrite(&s2e2, "fra")}; !slices.Equal(got, want) {
		t.Errorf("writes = %+v, want the show choice on every episode %+v", got, want)
	}
}
