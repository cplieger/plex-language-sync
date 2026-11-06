package tracksync

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"testing"
	"testing/synctest"
	"time"

	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/plex-language-sync/internal/ignore"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plex-language-sync/internal/testsupport/fakeapi"
	"github.com/cplieger/plexapi/v3"
)

// obsShow is the show every observe fixture belongs to.
const obsShow = "1001"

func obsAud(id int, lang, tag string) streams.Stream {
	return streams.Stream{ID: plexapi.FlexInt(id), StreamType: plexapi.StreamTypeAudio, LanguageCode: lang, LanguageTag: tag}
}

func obsSub(id int, lang string) streams.Stream {
	return streams.Stream{ID: plexapi.FlexInt(id), StreamType: plexapi.StreamTypeSubtitle, LanguageCode: lang, Codec: "srt"}
}

// obsEp is S<season>E<index> of show 1001 with rating key "2<season><index>",
// season key "30<season>", part 5<season><index>, and list as its streams,
// the ids in selected marked selected.
func obsEp(season, index int, list []streams.Stream, selected ...int) streams.Episode {
	list = slices.Clone(list)
	for i := range list {
		list[i].Selected = slices.Contains(selected, int(list[i].ID))
	}
	return streams.Episode{
		RatingKey:            fmt.Sprintf("2%d%02d", season, index),
		GrandparentRatingKey: obsShow,
		ParentRatingKey:      fmt.Sprintf("30%d", season),
		GrandparentTitle:     textsafe.NewUntrusted("Example Show"),
		ParentIndex:          streams.FlexInt(season),
		Index:                streams.FlexInt(index),
		Media:                []streams.Media{{Part: []streams.Part{{ID: 5000 + season*100 + index, Stream: list}}}},
	}
}

// played is ep as the user's play-time read gives it, with exactly the ids
// in selected selected.
func played(ep streams.Episode, selected ...int) *streams.Episode {
	list := slices.Clone(ep.Media[0].Part[0].Stream)
	for i := range list {
		list[i].Selected = slices.Contains(selected, int(list[i].ID))
	}
	ep.Media = []streams.Media{{Part: []streams.Part{{ID: ep.Media[0].Part[0].ID, Stream: list}}}}
	return &ep
}

// observeLib is a fake library of show 1001 served through one user's
// client, with a clock the test moves.
type observeLib struct {
	plx   *fakeapi.Plex
	cache *fakeapi.Cache
	s     *Syncer
	clock int64
}

func newObserveLib(t *testing.T, cfg Config, eps ...streams.Episode) *observeLib {
	t.Helper()
	l := &observeLib{
		plx: &fakeapi.Plex{
			ShowEpisodesByShow:  map[string][]streams.Episode{},
			SeasonEpisodesByKey: map[string][]streams.Episode{},
			EpisodeByKey:        map[string]*streams.Episode{},
		},
		cache: fakeapi.NewCache(),
		clock: 1000,
	}
	for i := range eps {
		ep := eps[i]
		l.plx.ShowEpisodesByShow[obsShow] = append(l.plx.ShowEpisodesByShow[obsShow], ep)
		l.plx.SeasonEpisodesByKey[ep.ParentRatingKey] = append(l.plx.SeasonEpisodesByKey[ep.ParentRatingKey], ep)
		l.plx.EpisodeByKey[ep.RatingKey] = &ep
	}
	l.s = newSyncer(cfg, l.plx, l.cache, &fakeUsers{})
	l.s.apply.now = func() time.Time { return time.Unix(l.clock, 0) }
	return l
}

func (l *observeLib) observe(t *testing.T, ep *streams.Episode) {
	t.Helper()
	if err := l.s.Observe(t.Context(), "alice", ep, "play"); err != nil {
		t.Fatalf("Observe = %v, want nil", err)
	}
}

// play is alice selecting the ids in selected on ep and playing it: her
// own read of ep returns that selection from then on.
func (l *observeLib) play(t *testing.T, ep streams.Episode, selected ...int) {
	t.Helper()
	p := played(ep, selected...)
	stored := *p
	l.plx.EpisodeByKey[ep.RatingKey] = &stored
	l.observe(t, p)
}

// writesOn returns the accepted writes on part.
func (l *observeLib) writesOn(part int) []fakeapi.Write {
	var out []fakeapi.Write
	for _, w := range l.plx.Writes() {
		if w.PartID == part {
			out = append(out, w)
		}
	}
	return out
}

func partOf(ep *streams.Episode) int { return ep.Media[0].Part[0].ID }

var allShow = Config{UpdateLevel: LevelShow, UpdateStrategy: StrategyAll}

func TestObserveMissingTrackKeepsIntent(t *testing.T) {
	t.Parallel()
	e1 := obsEp(1, 1, []streams.Stream{obsAud(9101, "jpn", ""), obsAud(9102, "eng", ""), obsSub(9111, "eng")}, 9101, 9111)
	e7 := obsEp(1, 7, []streams.Stream{obsAud(9701, "eng", ""), obsSub(9711, "eng")}, 9701)
	l := newObserveLib(t, allShow, e1, e7)

	l.play(t, e1, 9101, 9111)

	want := []fakeapi.Write{{Kind: "subtitle", PartID: partOf(&e7), StreamID: 9711}}
	if got := l.writesOn(partOf(&e7)); !slices.Equal(got, want) {
		t.Errorf("writes on the episode without Japanese = %+v, want only its English subtitle %+v", got, want)
	}
	rec, ok := l.cache.AppliedFor("alice", e7.RatingKey)
	if !ok || rec.AudioID != 9701 || rec.SubtitleID != 9711 {
		t.Errorf("record on the episode without Japanese = %+v, %v; want audio 9701 and subtitle 9711", rec, ok)
	}

	l.clock = 2000
	writes := len(l.plx.Writes())
	l.play(t, e7, 9701, 9711)

	in, ok := l.cache.IntentFor("alice", obsShow)
	if !ok || in.Audio.LanguageCode != "jpn" || in.ObservedAt != 1000 {
		t.Errorf("intent after playing the dub the app left = %+v, %v; want the Japanese choice of 1000", in, ok)
	}
	if got := len(l.plx.Writes()); got != writes {
		t.Errorf("playing the dub the app left wrote %d more streams, want none", got-writes)
	}
}

func TestObserveSubstituteKeepsReference(t *testing.T) {
	t.Parallel()
	e1 := obsEp(1, 1, []streams.Stream{obsAud(9101, "por", "pt-BR"), obsAud(9102, "eng", "")}, 9101)
	e2 := obsEp(1, 2, []streams.Stream{obsAud(9201, "por", "pt-PT"), obsAud(9202, "eng", "")}, 9202)
	l := newObserveLib(t, allShow, e1, e2)

	l.play(t, e1, 9101)
	want := []fakeapi.Write{{Kind: "audio", PartID: partOf(&e2), StreamID: 9201}}
	if got := l.writesOn(partOf(&e2)); !slices.Equal(got, want) {
		t.Fatalf("writes on the pt-PT episode = %+v, want %+v", got, want)
	}

	l.clock = 2000
	l.play(t, e2, 9201)

	in, _ := l.cache.IntentFor("alice", obsShow)
	if in.Audio.LanguageTag != "pt-BR" || in.ObservedAt != 1000 {
		t.Errorf("intent after playing the substitute = tag %q at %d, want pt-BR at 1000", in.Audio.LanguageTag, in.ObservedAt)
	}
}

func TestObserveSwitchBackWithinMinutes(t *testing.T) {
	t.Parallel()
	e1 := obsEp(1, 1, []streams.Stream{obsAud(9101, "jpn", ""), obsAud(9102, "eng", "")}, 9101)
	e2 := obsEp(1, 2, []streams.Stream{obsAud(9201, "jpn", ""), obsAud(9202, "eng", "")}, 9202)
	l := newObserveLib(t, allShow, e1, e2)

	l.play(t, e1, 9101)
	l.clock = 1060
	l.play(t, e1, 9102)
	l.clock = 1120
	l.play(t, e1, 9101)

	in, _ := l.cache.IntentFor("alice", obsShow)
	if in.Audio.LanguageCode != "jpn" || in.ObservedAt != 1120 {
		t.Errorf("intent = %q at %d, want jpn at 1120", in.Audio.LanguageCode, in.ObservedAt)
	}
	got := l.writesOn(partOf(&e2))
	if len(got) == 0 || got[len(got)-1] != (fakeapi.Write{Kind: "audio", PartID: partOf(&e2), StreamID: 9201}) {
		t.Errorf("writes on E2 = %+v, want the last to put Japanese back", got)
	}
}

func TestObserveTwoChoicesInOneSecond(t *testing.T) {
	t.Parallel()
	e1 := obsEp(1, 1, []streams.Stream{obsAud(9101, "jpn", ""), obsAud(9102, "eng", "")}, 9101)
	e2 := obsEp(1, 2, []streams.Stream{obsAud(9201, "jpn", ""), obsAud(9202, "eng", "")}, 9202)
	l := newObserveLib(t, allShow, e1, e2)

	for _, step := range []struct{ played, wantE2 int }{{9101, 9201}, {9102, 9202}, {9101, 9201}} {
		l.play(t, e1, step.played)
		if rec, _ := l.cache.AppliedFor("alice", e2.RatingKey); rec.AudioID != step.wantE2 {
			t.Errorf("after choosing %d, E2 is recorded with audio %d, want %d", step.played, rec.AudioID, step.wantE2)
		}
	}
	if in, _ := l.cache.IntentFor("alice", obsShow); in.Audio.LanguageCode != "jpn" {
		t.Errorf("intent audio = %q, want jpn", in.Audio.LanguageCode)
	}
}

func TestObserveRepeatDoesNothing(t *testing.T) {
	t.Parallel()
	e1 := obsEp(1, 1, []streams.Stream{obsAud(9101, "jpn", ""), obsAud(9102, "eng", "")}, 9102)
	e2 := obsEp(1, 2, []streams.Stream{obsAud(9201, "jpn", ""), obsAud(9202, "eng", "")}, 9202)
	l := newObserveLib(t, allShow, e1, e2)

	l.play(t, e1, 9101)
	calls, writes := len(l.plx.CallNames()), len(l.plx.Writes())
	l.clock = 1010
	l.play(t, e1, 9101)

	if got := l.plx.CallNames()[calls:]; len(got) != 0 {
		t.Errorf("a re-announced play made requests %v, want none", got)
	}
	if got := len(l.plx.Writes()); got != writes {
		t.Errorf("a re-announced play wrote %d streams, want none", got-writes)
	}
	if in, _ := l.cache.IntentFor("alice", obsShow); in.ObservedAt != 1000 {
		t.Errorf("intent observed at %d, want 1000 (a repeat records nothing)", in.ObservedAt)
	}
}

func TestObserveIgnoredShowRecordsNothing(t *testing.T) {
	t.Parallel()
	e1 := obsEp(1, 1, []streams.Stream{obsAud(9101, "jpn", ""), obsAud(9102, "eng", "")}, 9102)
	e2 := obsEp(1, 2, []streams.Stream{obsAud(9201, "jpn", ""), obsAud(9202, "eng", "")}, 9202)
	l := newObserveLib(t, allShow, e1, e2)
	l.plx.ShowMetadataByKey = map[string]*plex.Show{obsShow: {Label: []streams.Label{{Tag: "SKIP"}}}}
	cfg := allShow
	cfg.Ignore = ignore.New(ignore.Config{Reader: l.plx, Labels: []string{"SKIP"}})
	cfg.LanguageProfiles = true
	l.s = newSyncer(cfg, l.plx, l.cache, &fakeUsers{})

	l.play(t, e1, 9101)

	if _, ok := l.cache.IntentFor("alice", obsShow); ok {
		t.Error("an ignored show recorded an intent")
	}
	if got, ok := l.cache.SubtitleLangForAudio("alice", "jpn"); ok {
		t.Errorf("an ignored show taught the profile jpn->%q", got)
	}
	if _, ok := l.cache.AppliedFor("alice", e2.RatingKey); ok {
		t.Error("an ignored show recorded a write")
	}
	if got := l.plx.CallNames(); !slices.Equal(got, []string{"ShowMetadata:" + obsShow}) {
		t.Errorf("requests = %v, want only the show's labels", got)
	}
}

func TestObserveFirstPlayOfSeededEpisodeRecordsIntent(t *testing.T) {
	t.Parallel()
	e1 := obsEp(1, 1, []streams.Stream{obsAud(9101, "jpn", ""), obsAud(9102, "eng", "")}, 9102)
	e13 := obsEp(1, 13, []streams.Stream{obsAud(91301, "jpn", ""), obsAud(91302, "eng", "")}, 91301)
	l := newObserveLib(t, allShow, e1, e13)
	seed := streams.Basis{From: "bob", Version: 500, Source: streams.SourceHousehold}
	l.cache.RecordApplied("alice", e13.RatingKey, streams.Applied{Basis: seed, At: 600, PartID: partOf(&e13), AudioID: 91301})

	l.play(t, e13, 91301)

	in, ok := l.cache.IntentFor("alice", obsShow)
	if !ok || in.Audio.LanguageCode != "jpn" || in.Anchor.RatingKey != e13.RatingKey || in.Anchor.AudioID != 91301 {
		t.Errorf("intent = %+v, %v; want Japanese anchored on E13 with its audio id", in, ok)
	}
	want := []fakeapi.Write{{Kind: "audio", PartID: partOf(&e1), StreamID: 9101}}
	if got := l.writesOn(partOf(&e1)); !slices.Equal(got, want) {
		t.Errorf("writes on E1 = %+v, want %+v", got, want)
	}
}

func TestObserveNoSelectedAudioRecordsNothing(t *testing.T) {
	t.Parallel()
	e1 := obsEp(1, 1, []streams.Stream{obsAud(9101, "jpn", ""), obsSub(9111, "eng")}, 9101)
	cfg := allShow
	cfg.LanguageProfiles = true

	t.Run("no intent", func(t *testing.T) {
		t.Parallel()
		l := newObserveLib(t, cfg, e1)
		l.play(t, e1, 9111)
		if _, ok := l.cache.IntentFor("alice", obsShow); ok {
			t.Error("a play with no audio selected recorded an intent")
		}
		if _, ok := l.cache.SubtitleLangForAudio("alice", "jpn"); ok {
			t.Error("a play with no audio selected taught the profile")
		}
		if got := l.plx.CallNames(); len(got) != 0 {
			t.Errorf("requests = %v, want none", got)
		}
	})

	t.Run("an intent and a current record", func(t *testing.T) {
		t.Parallel()
		l := newObserveLib(t, cfg, e1)
		l.play(t, e1, 9101, 9111)
		before, _ := l.cache.IntentFor("alice", obsShow)
		l.clock = 2000
		l.play(t, e1, 9111)
		after, _ := l.cache.IntentFor("alice", obsShow)
		if after.ObservedAt != before.ObservedAt || after.Audio != before.Audio {
			t.Errorf("intent = %+v, want it unchanged at %+v", after, before)
		}
	})
}

func TestObserveCommentaryChoiceSkipsProfile(t *testing.T) {
	t.Parallel()
	commentary := obsAud(9101, "eng", "")
	commentary.Title = "Director Commentary"
	e1 := obsEp(1, 1, []streams.Stream{commentary, obsAud(9102, "eng", "")}, 9102)
	cfg := allShow
	cfg.LanguageProfiles = true
	l := newObserveLib(t, cfg, e1)

	l.play(t, e1, 9101)

	in, ok := l.cache.IntentFor("alice", obsShow)
	if !ok || in.Audio.Title != "Director Commentary" {
		t.Errorf("intent = %+v, %v; want the commentary track recorded", in, ok)
	}
	if got, ok := l.cache.SubtitleLangForAudio("alice", "eng"); ok {
		t.Errorf("a commentary choice taught the profile eng->%q", got)
	}
}

// legacyIntent is a Japanese choice for show 1001 with no subtitle, as an
// intent written before anchors existed.
func legacyIntent(observedAt int64) *streams.Intent {
	jpn := obsAud(0, "jpn", "")
	return streams.NewIntent(streams.Pair{Audio: &jpn}, streams.Anchor{}, observedAt)
}

func TestObserveSeasonFallsBackToShowKey(t *testing.T) {
	t.Parallel()
	s1 := obsEp(1, 1, []streams.Stream{obsAud(9101, "jpn", ""), obsAud(9102, "eng", "")}, 9101)
	s2e1 := obsEp(2, 1, []streams.Stream{obsAud(9211, "jpn", ""), obsAud(9212, "eng", "")}, 9211)
	s2e2 := obsEp(2, 2, []streams.Stream{obsAud(9221, "jpn", ""), obsAud(9222, "eng", "")}, 9221)
	l := newObserveLib(t, Config{UpdateLevel: LevelSeason, UpdateStrategy: StrategyAll}, s1, s2e1, s2e2)
	l.cache.RecordIntent("alice", obsShow, legacyIntent(500))

	t.Run("an unchanged play is the show's choice", func(t *testing.T) {
		l.play(t, s2e1, 9211)
		if _, ok := l.cache.IntentFor("alice", "1001/s2"); ok {
			t.Error("an unchanged play under a show record recorded a season choice")
		}
		if got := l.plx.Writes(); len(got) != 0 {
			t.Errorf("writes = %+v, want none", got)
		}
	})

	t.Run("a different audio records the season", func(t *testing.T) {
		l.clock = 2000
		l.play(t, s2e1, 9212)
		in, ok := l.cache.IntentFor("alice", "1001/s2")
		if !ok || in.Audio.LanguageCode != "eng" || in.ObservedAt != 2000 {
			t.Errorf("season intent = %+v, %v; want English at 2000", in, ok)
		}
		if show, _ := l.cache.IntentFor("alice", obsShow); show.ObservedAt != 500 || show.Audio.LanguageCode != "jpn" {
			t.Errorf("show intent = %+v, want the Japanese record of 500 left in place", show)
		}
		want := []fakeapi.Write{{Kind: "audio", PartID: partOf(&s2e2), StreamID: 9222}}
		if got := l.plx.Writes(); !slices.Equal(got, want) {
			t.Errorf("writes = %+v, want only S2E2's English %+v", got, want)
		}
		if n := countCalls(l.plx.CallNames(), "ShowEpisodes:"+obsShow); n != 0 {
			t.Errorf("a season choice listed the whole show %d times", n)
		}
	})
}

func TestObserveSeasonLevelReadsAnchoredShowKey(t *testing.T) {
	t.Parallel()
	s1e3 := obsEp(1, 3, []streams.Stream{obsAud(9131, "jpn", ""), obsAud(9132, "eng", "")}, 9131)
	s2e1 := obsEp(2, 1, []streams.Stream{obsAud(9211, "jpn", ""), obsAud(9212, "eng", "")}, 9211)
	s2e2 := obsEp(2, 2, []streams.Stream{obsAud(9221, "jpn", ""), obsAud(9222, "eng", "")}, 9221)
	s2e3 := obsEp(2, 3, []streams.Stream{obsAud(9231, "jpn", ""), obsAud(9232, "eng", "")}, 9231)
	l := newObserveLib(t, Config{UpdateLevel: LevelSeason, UpdateStrategy: StrategyAll}, s1e3, s2e1, s2e2, s2e3)
	jpn := obsAud(0, "jpn", "")
	l.cache.RecordIntent("alice", obsShow, streams.NewIntent(streams.Pair{Audio: &jpn},
		streams.Anchor{RatingKey: s1e3.RatingKey, Season: 1, Index: 3, AudioID: 9131}, 500))
	l.cache.RecordApplied("alice", s2e2.RatingKey, streams.Applied{
		Basis: streams.Basis{Version: 500, Source: streams.SourceIntent}, At: 600, PartID: partOf(&s2e2), AudioID: 9221,
	})

	t.Run("an unchanged play is an echo", func(t *testing.T) {
		l.play(t, s2e2, 9221)
		if _, ok := l.cache.IntentFor("alice", "1001/s2"); ok {
			t.Error("an unchanged play under the show record recorded a season choice")
		}
		if got := l.plx.CallNames(); len(got) != 0 {
			t.Errorf("requests = %v, want none", got)
		}
	})

	t.Run("a different audio records the season", func(t *testing.T) {
		l.clock = 2000
		l.play(t, s2e2, 9222)
		in, ok := l.cache.IntentFor("alice", "1001/s2")
		if !ok || in.Anchor.RatingKey != s2e2.RatingKey || in.Anchor.AudioID != 9222 {
			t.Errorf("season intent = %+v, %v; want anchored on S2E2 with its English id", in, ok)
		}
		if show, _ := l.cache.IntentFor("alice", obsShow); show.ObservedAt != 500 {
			t.Errorf("show intent observed at %d, want the record of 500 left in place", show.ObservedAt)
		}
		want := []fakeapi.Write{
			{Kind: "audio", PartID: partOf(&s2e1), StreamID: 9212},
			{Kind: "audio", PartID: partOf(&s2e3), StreamID: 9232},
		}
		if got := l.plx.Writes(); !slices.Equal(got, want) {
			t.Errorf("writes = %+v, want S2E1 and S2E3 on English %+v", got, want)
		}
		if n := countCalls(l.plx.CallNames(), "SeasonEpisodes:302"); n != 1 {
			t.Errorf("season 2 listed %d times, want 1", n)
		}
	})
}

func TestIntentForNewestOfTwoKeys(t *testing.T) {
	t.Parallel()
	s1 := obsEp(1, 1, nil)
	s2 := obsEp(2, 1, nil)
	tests := []struct {
		name             string
		showAt, seasonAt int64
		ep               *streams.Episode
		wantKey          string
		wantObservedAt   int64
	}{
		{name: "the newer season record governs its season", showAt: 100, seasonAt: 101, ep: &s2, wantKey: "1001/s2", wantObservedAt: 101},
		{name: "another season reads the show record", showAt: 100, seasonAt: 101, ep: &s1, wantKey: obsShow, wantObservedAt: 100},
		{name: "a newer show record governs every season", showAt: 102, seasonAt: 101, ep: &s2, wantKey: obsShow, wantObservedAt: 102},
		{name: "a tie goes to the show record", showAt: 100, seasonAt: 100, ep: &s2, wantKey: obsShow, wantObservedAt: 100},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := newObserveLib(t, allShow)
			l.cache.RecordIntent("alice", obsShow, legacyIntent(tc.showAt))
			l.cache.RecordIntent("alice", "1001/s2", legacyIntent(tc.seasonAt))
			key, in, ok := l.s.intentFor("alice", tc.ep)
			if !ok || key.String() != tc.wantKey || in.ObservedAt != tc.wantObservedAt {
				t.Errorf("intentFor = %q, %d, %v; want %q, %d, true", key, in.ObservedAt, ok, tc.wantKey, tc.wantObservedAt)
			}
		})
	}

	t.Run("no record", func(t *testing.T) {
		t.Parallel()
		l := newObserveLib(t, allShow)
		if key, _, ok := l.s.intentFor("alice", &s2); ok {
			t.Errorf("intentFor = %q, true; want no record", key)
		}
	})
}

func TestObserveLockCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e1 := obsEp(1, 1, []streams.Stream{obsAud(9101, "jpn", ""), obsAud(9102, "eng", "")}, 9102)
		l := newObserveLib(t, allShow, e1)
		unlock, err := l.s.apply.lock(t.Context(), "alice", obsShow)
		if err != nil {
			t.Fatalf("lock = %v", err)
		}
		defer unlock()

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- l.s.Observe(ctx, "alice", played(e1, 9101), "play") }()
		synctest.Wait()
		cancel()

		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("Observe = %v, want context.Canceled", err)
		}
		if _, ok := l.cache.IntentFor("alice", obsShow); ok {
			t.Error("a play whose lock wait was cancelled recorded an intent")
		}
		if got := l.plx.CallNames(); len(got) != 0 {
			t.Errorf("requests = %v, want none", got)
		}
	})
}
