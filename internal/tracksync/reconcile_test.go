package tracksync

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"testing/synctest"

	"github.com/cplieger/go/textsafe"
	"github.com/cplieger/plex-language-sync/internal/ignore"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plex-language-sync/internal/testsupport/fakeapi"
	"github.com/cplieger/plexapi/v3"
)

// ---------------------------------------------------------------------------
// Event plane: Observe records intents
// ---------------------------------------------------------------------------

// TestObserve_RecordsIntent pins the event-plane knowledge
// creation: a resolved play observation writes the (user, show) intent —
// audio language plus the "no subtitles" nil marker — so the reconcile
// plane and new-episode seeding can re-apply it later without re-deriving
// the user's choice from later library reads.
func TestObserve_RecordsIntent(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{"42": {listEp("2")}},
		EpisodeByKey:       map[string]*streams.Episode{"2": targetNeedingAudioSwitch("2")},
	}
	c := fakeapi.NewCache()
	s := newSyncer(Config{UpdateLevel: LevelShow}, plx, c, &fakeUsers{})
	ref := refWithSelectedAudio("jpn", "42", "7", 1, 1)

	observeOK(t, s, ref)

	intent, ok := c.IntentFor("1", "42")
	if !ok {
		t.Fatal("Observe did not record an intent for (user 1, show 42)")
	}
	if intent.Audio.LanguageCode != "jpn" {
		t.Errorf("intent audio = %q, want jpn", intent.Audio.LanguageCode)
	}
	if intent.Subtitle != nil {
		t.Errorf("intent subtitle = %+v, want nil (reference has no selected subtitle)", intent.Subtitle)
	}
	if intent.ObservedAt <= 0 {
		t.Errorf("intent ObservedAt = %d, want a positive unix timestamp", intent.ObservedAt)
	}
}

// TestObserve_IgnoredShowRecordsNoIntent extends the
// ignore-before-learn rule to the intent ledger: an ignored show must not
// contribute intents any more than profiles.
func TestObserve_IgnoredShowRecordsNoIntent(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		ShowMetadataByKey: map[string]*plex.Show{
			"42": {Label: []streams.Label{{Tag: "SKIP"}}},
		},
	}
	c := fakeapi.NewCache()
	policy := ignore.New(ignore.Config{Reader: plx, Labels: []string{"SKIP"}})
	s := newSyncer(Config{UpdateLevel: LevelShow, Ignore: policy}, plx, c, &fakeUsers{})
	ref := refWithSelectedAudio("jpn", "42", "7", 1, 1)

	observeOK(t, s, ref)

	if _, ok := c.IntentFor("1", "42"); ok {
		t.Error("intent recorded for an ignored show; the ignore gate must precede intent recording")
	}
}

// TestObserve_CommentaryAudioStillRecordsIntent pins the
// deliberate asymmetry between the two knowledge stores: profile learning
// skips commentary/descriptive tracks (cross-show generalization), but the
// per-show intent records them — a deliberate atypical choice for THIS
// show is exactly what the ledger should remember.
func TestObserve_CommentaryAudioStillRecordsIntent(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{"42": {}},
	}
	c := fakeapi.NewCache()
	s := newSyncer(Config{UpdateLevel: LevelShow, LanguageProfiles: true}, plx, c, &fakeUsers{})
	ref := &streams.Episode{
		RatingKey:            "1",
		GrandparentRatingKey: "42",
		ParentRatingKey:      "7",
		GrandparentTitle:     textsafe.NewUntrusted("Show"),
		Media: []streams.Media{{Part: []streams.Part{{ID: 100, Stream: []streams.Stream{
			{
				ID: 11, StreamType: plexapi.StreamTypeAudio, LanguageCode: "eng",
				Title: "Director Commentary", Selected: true,
			},
		}}}}},
	}

	observeOK(t, s, ref)

	if _, ok := c.SubtitleLangForAudio("1", "eng"); ok {
		t.Error("commentary track was learned into the cross-show profile; learning must skip descriptive tracks")
	}
	intent, ok := c.IntentFor("1", "42")
	if !ok {
		t.Fatal("commentary track did not record a per-show intent; intents must record deliberate atypical choices")
	}
	if intent.Audio.Title != "Director Commentary" {
		t.Errorf("intent audio title = %q, want the commentary title preserved", intent.Audio.Title)
	}
}

// ---------------------------------------------------------------------------
// History replay: Reconcile
// ---------------------------------------------------------------------------

// seedIntent records a jpn-audio/no-subtitle intent for (user 1, show 42)
// observed at the given timestamp.
func seedIntent(c *fakeapi.Cache, observedAt int64) {
	c.RecordIntent("1", "42", streams.NewIntent(streams.Pair{Audio: &streams.Stream{LanguageCode: "jpn"}, Subtitle: nil}, streams.Anchor{}, observedAt))
}

// replayedEpisode builds the episode a history replay fetched, its current
// selection English, which the replay must neither apply nor learn.
func replayedEpisode() *streams.Episode {
	return &streams.Episode{
		RatingKey:            "1",
		GrandparentRatingKey: "42",
		ParentRatingKey:      "7",
		GrandparentTitle:     textsafe.NewUntrusted("Show"),
		ParentIndex:          1,
		Index:                1,
		Media: []streams.Media{{Part: []streams.Part{{ID: 100, Stream: []streams.Stream{
			{ID: 10, StreamType: plexapi.StreamTypeAudio, LanguageCode: "eng", Selected: true},
			{ID: 11, StreamType: plexapi.StreamTypeAudio, LanguageCode: "jpn"},
		}}}}},
	}
}

func reconcileOK(t *testing.T, s *Syncer, ep *streams.Episode) {
	t.Helper()
	if err := s.Reconcile(t.Context(), "1", ep, "scheduler"); err != nil {
		t.Fatalf("Reconcile = %v, want nil", err)
	}
}

func TestReconcileAppliesRecordedIntentNotCurrentSelection(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{"42": {listEp("2")}},
		EpisodeByKey:       map[string]*streams.Episode{"2": targetNeedingAudioSwitch("2")},
	}
	c := fakeapi.NewCache()
	seedIntent(c, 1000)
	s := newSyncer(Config{UpdateLevel: LevelShow}, plx, c, &fakeUsers{})

	reconcileOK(t, s, replayedEpisode())

	want := []fakeapi.Write{{Kind: "audio", PartID: 100, StreamID: 11}}
	if got := plx.Writes(); !slices.Equal(got, want) {
		t.Errorf("writes = %+v, want the recorded Japanese %+v", got, want)
	}
	if _, ok := c.SubtitleLangForAudio("1", "eng"); ok {
		t.Error("the replay learned a profile from the replayed episode's selection")
	}
	if _, ok := c.SubtitleLangForAudio("1", "jpn"); ok {
		t.Error("the replay learned a profile from the recorded intent")
	}
}

func TestReconcileNoIntentSkips(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		ShowEpisodesByShow: map[string][]streams.Episode{"42": {listEp("2")}},
	}
	s := newSyncer(Config{UpdateLevel: LevelShow}, plx, fakeapi.NewCache(), &fakeUsers{})

	reconcileOK(t, s, replayedEpisode())

	if calls := plx.CallNames(); len(calls) != 0 {
		t.Errorf("requests = %v, want none without a recorded intent", calls)
	}
}

func TestReconcileIgnoredShowSkips(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		ShowMetadataByKey:  map[string]*plex.Show{"42": {Label: []streams.Label{{Tag: "SKIP"}}}},
		ShowEpisodesByShow: map[string][]streams.Episode{"42": {listEp("2")}},
	}
	c := fakeapi.NewCache()
	seedIntent(c, 1000)
	policy := ignore.New(ignore.Config{Reader: plx, Labels: []string{"SKIP"}})
	s := newSyncer(Config{UpdateLevel: LevelShow, Ignore: policy}, plx, c, &fakeUsers{})

	reconcileOK(t, s, replayedEpisode())

	if got := plx.CallNames(); !slices.Equal(got, []string{"ShowMetadata:42"}) {
		t.Errorf("requests = %v, want only the show's labels", got)
	}
}

func TestReconcileLockCancelled(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		e1 := obsEp(1, 1, []streams.Stream{obsAud(9101, "jpn", ""), obsAud(9102, "eng", "")}, 9102)
		l := newObserveLib(t, allShow, e1)
		l.cache.RecordIntent("alice", obsShow, legacyIntent(500))
		unlock, err := l.s.apply.lock(t.Context(), "alice", obsShow)
		if err != nil {
			t.Fatalf("lock = %v", err)
		}
		defer unlock()

		ctx, cancel := context.WithCancel(t.Context())
		done := make(chan error, 1)
		go func() { done <- l.s.Reconcile(ctx, "alice", &e1, "scheduler") }()
		synctest.Wait()
		cancel()

		if err := <-done; !errors.Is(err, context.Canceled) {
			t.Errorf("Reconcile = %v, want context.Canceled", err)
		}
		if got := l.plx.CallNames(); len(got) != 0 {
			t.Errorf("requests = %v, want none", got)
		}
	})
}

// The fixtures below are show 1001 with Japanese, English and French audio on
// every episode, French selected, so each write the replay makes is visible.

func triAudio(season, index int) streams.Episode {
	base := 9000 + season*100 + index*10
	return obsEp(season, index, []streams.Stream{
		obsAud(base+1, "jpn", ""), obsAud(base+2, "eng", ""), obsAud(base+3, "fra", ""),
	}, base+3)
}

// audioOf is the id of ep's audio stream in lang.
func audioOf(ep *streams.Episode, lang string) int {
	for _, st := range streams.Audio(ep) {
		if st.LanguageCode == lang {
			return int(st.ID)
		}
	}
	return 0
}

// store puts ep in every list and read of l's library in place of the
// episode with its rating key.
func (l *observeLib) store(ep streams.Episode) {
	replace := func(list []streams.Episode) {
		for i := range list {
			if list[i].RatingKey == ep.RatingKey {
				list[i] = ep
			}
		}
	}
	replace(l.plx.ShowEpisodesByShow[obsShow])
	replace(l.plx.SeasonEpisodesByKey[ep.ParentRatingKey])
	stored := ep
	l.plx.EpisodeByKey[ep.RatingKey] = &stored
}

// view is alice selecting the ids in selected on ep and finishing it at at,
// a play the app did not observe.
func (l *observeLib) view(ep streams.Episode, at int64, selected ...int) {
	p := played(ep, selected...)
	p.LastViewedAt = streams.FlexInt(at)
	l.store(*p)
}

func (l *observeLib) reconcile(t *testing.T, ep streams.Episode) {
	t.Helper()
	if err := l.s.Reconcile(t.Context(), "alice", &ep, "scheduler"); err != nil {
		t.Fatalf("Reconcile = %v, want nil", err)
	}
}

// audioWritesTo maps each part the replay wrote audio on to the stream it set.
func (l *observeLib) audioWritesTo(from int) map[int]int {
	out := map[int]int{}
	for _, w := range l.plx.Writes()[from:] {
		if w.Kind == "audio" {
			out[w.PartID] = w.StreamID
		}
	}
	return out
}

func recordFor(c *fakeapi.Cache, key, lang string, anchor streams.Anchor, at int64) {
	audio := obsAud(0, lang, "")
	c.RecordIntent("alice", key, streams.NewIntent(streams.Pair{Audio: &audio}, anchor, at))
}

func TestReconcileProtectsOtherTarget(t *testing.T) {
	t.Parallel()
	e1, e4, e6 := triAudio(1, 1), triAudio(1, 4), triAudio(1, 6)
	for _, tc := range []struct {
		name       string
		keepRecord bool
	}{{"with the record of the app's write", true}, {"with no record", false}} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := newObserveLib(t, allShow, e1, e4, e6)
			l.play(t, e1, audioOf(&e1, "jpn"))
			if !tc.keepRecord {
				l.cache.ForgetApplied("alice", e6.RatingKey)
			}
			l.view(e6, 1100, audioOf(&e6, "eng"))
			l.clock = 2000
			from := len(l.plx.Writes())

			l.reconcile(t, e4)

			if got := l.writesOn(partOf(&e6)); len(got) != 1 {
				t.Errorf("writes on E6 = %+v, want only the first choice's, none from the replay", got)
			}
			if got := l.plx.Writes()[from:]; len(got) != 0 {
				t.Errorf("the replay wrote %+v, want nothing", got)
			}
		})
	}
}

func TestReconcileSkipsReplayedItemPlayedAfterIntent(t *testing.T) {
	t.Parallel()
	e4, e5 := triAudio(1, 4), triAudio(1, 5)
	for _, tc := range []struct {
		name      string
		viewed    int64
		wantE4Jpn bool
	}{
		{"a view after the choice", 2000, false},
		{"the view the choice was made on", 1000, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := newObserveLib(t, allShow, e4, e5)
			recordFor(l.cache, obsShow, "jpn", streams.Anchor{}, 1000)
			l.view(e4, tc.viewed, audioOf(&e4, "fra"))
			l.clock = 3000

			l.reconcile(t, e4)

			got := l.audioWritesTo(0)
			if _, wrote := got[partOf(&e4)]; wrote != tc.wantE4Jpn {
				t.Errorf("replayed item written = %v, want %v (writes %v)", wrote, tc.wantE4Jpn, got)
			}
			if got[partOf(&e5)] != audioOf(&e5, "jpn") {
				t.Errorf("writes = %v, want E5 put on Japanese", got)
			}
		})
	}
}

func TestReconcileProtectsViewOutsideWindow(t *testing.T) {
	t.Parallel()
	e4, e6 := triAudio(1, 4), triAudio(1, 6)
	l := newObserveLib(t, allShow, e4, e6)
	recordFor(l.cache, obsShow, "jpn", streams.Anchor{}, 1000)
	l.view(e6, 1500, audioOf(&e6, "eng"))
	l.clock = 3000

	l.reconcile(t, e4)

	want := map[int]int{partOf(&e4): audioOf(&e4, "jpn")}
	if got := l.audioWritesTo(0); !maps.Equal(got, want) {
		t.Errorf("writes = %v, want only E4 put on Japanese %v", got, want)
	}
}

func TestReconcileShowLevelUsesNewestRecord(t *testing.T) {
	t.Parallel()
	s1e1, s1e2, s2e1, s2e2 := triAudio(1, 1), triAudio(1, 2), triAudio(2, 1), triAudio(2, 2)
	all := []streams.Episode{s1e1, s1e2, s2e1, s2e2}
	on := func(lang string, eps ...streams.Episode) map[int]int {
		out := map[int]int{}
		for i := range eps {
			out[partOf(&eps[i])] = audioOf(&eps[i], lang)
		}
		return out
	}
	tests := []struct {
		name     string
		showAt   int64
		replayed streams.Episode
		want     map[int]int
	}{
		{"a season item takes the newer season choice", 1000, s2e1, on("eng", s2e1, s2e2)},
		{"an older show record leaves the newer season alone", 1000, s1e1, on("jpn", s1e1, s1e2)},
		{"a newer show record covers every season", 1002, s2e1, on("jpn", all...)},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := newObserveLib(t, allShow, all...)
			recordFor(l.cache, obsShow, "jpn", streams.Anchor{}, tc.showAt)
			recordFor(l.cache, "1001/s2", "eng", streams.Anchor{}, 1001)
			l.clock = 3000

			l.reconcile(t, tc.replayed)

			if got := l.audioWritesTo(0); !maps.Equal(got, tc.want) {
				t.Errorf("writes = %v, want %v", got, tc.want)
			}
		})
	}
}

func TestReconcileListFollowsKey(t *testing.T) {
	t.Parallel()
	s2e1, s2e2 := triAudio(2, 1), triAudio(2, 2)
	tests := []struct {
		name, level, key         string
		wantShowList, wantSeason int
	}{
		{"a season record at show level reads its season", LevelShow, "1001/s2", 0, 1},
		{"a show record at season level reads the show", LevelSeason, obsShow, 1, 0},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := newObserveLib(t, Config{UpdateLevel: tc.level, UpdateStrategy: StrategyAll}, s2e1, s2e2)
			recordFor(l.cache, tc.key, "jpn", streams.Anchor{}, 1000)

			l.reconcile(t, s2e1)

			calls := l.plx.CallNames()
			if n := countCalls(calls, "ShowEpisodes:"+obsShow); n != tc.wantShowList {
				t.Errorf("show listed %d times, want %d (calls %v)", n, tc.wantShowList, calls)
			}
			if n := countCalls(calls, "SeasonEpisodes:302"); n != tc.wantSeason {
				t.Errorf("season 2 listed %d times, want %d (calls %v)", n, tc.wantSeason, calls)
			}
			if got := len(l.audioWritesTo(0)); got != 2 {
				t.Errorf("wrote audio on %d episodes, want 2", got)
			}
		})
	}
}

func TestReconcileRepairsFailedWrite(t *testing.T) {
	t.Parallel()
	e1, e2 := triAudio(1, 1), triAudio(1, 2)
	l := newObserveLib(t, allShow, e1, e2)
	recordFor(l.cache, obsShow, "jpn", streams.Anchor{}, 1000)
	// What a write that failed after the audio PUT leaves: what the part holds, no basis.
	l.cache.RecordApplied("alice", e2.RatingKey, streams.Applied{At: 1000, PartID: partOf(&e2), AudioID: audioOf(&e2, "fra")})
	l.clock = 3000

	l.reconcile(t, e1)

	want := map[int]int{partOf(&e1): audioOf(&e1, "jpn"), partOf(&e2): audioOf(&e2, "jpn")}
	if got := l.audioWritesTo(0); !maps.Equal(got, want) {
		t.Errorf("writes = %v, want both episodes put on Japanese %v", got, want)
	}
}

func TestReconcileSeasonLevel(t *testing.T) {
	t.Parallel()
	s1e1, s1e3, s2e1 := triAudio(1, 1), triAudio(1, 3), triAudio(2, 1)
	l := newObserveLib(t, Config{UpdateLevel: LevelSeason, UpdateStrategy: StrategyAll}, s1e1, s1e3, s2e1)
	recordFor(l.cache, "1001/s1", "jpn", streams.Anchor{}, 1000)
	recordFor(l.cache, "1001/s2", "eng", streams.Anchor{}, 1001)

	l.reconcile(t, s1e3)

	want := map[int]int{partOf(&s1e1): audioOf(&s1e1, "jpn"), partOf(&s1e3): audioOf(&s1e3, "jpn")}
	if got := l.audioWritesTo(0); !maps.Equal(got, want) {
		t.Errorf("writes = %v, want season 1 on Japanese and season 2 untouched %v", got, want)
	}
}

func TestReconcileNextFromAnchor(t *testing.T) {
	t.Parallel()
	e1, e2, e3, e9, e10 := triAudio(1, 1), triAudio(1, 2), triAudio(1, 3), triAudio(1, 9), triAudio(1, 10)
	l := newObserveLib(t, Config{UpdateLevel: LevelShow, UpdateStrategy: StrategyNext}, e1, e2, e3, e9, e10)
	recordFor(l.cache, obsShow, "jpn", streams.Anchor{RatingKey: e2.RatingKey, Season: 1, Index: 2, AudioID: audioOf(&e2, "jpn")}, 1000)

	l.reconcile(t, e9)

	want := map[int]int{}
	for _, ep := range []*streams.Episode{&e3, &e9, &e10} {
		want[partOf(ep)] = audioOf(ep, "jpn")
	}
	if got := l.audioWritesTo(0); !maps.Equal(got, want) {
		t.Errorf("writes = %v, want every episode after the anchor E2 %v", got, want)
	}
}

func TestReconcileLegacyIntentScopesFromItem(t *testing.T) {
	t.Parallel()
	e1, e3, e4 := triAudio(1, 1), triAudio(1, 3), triAudio(1, 4)
	tests := []struct {
		name     string
		strategy string
		want     []*streams.Episode
	}{
		{"next keeps the episodes after the replayed one", StrategyNext, []*streams.Episode{&e4}},
		{"all keeps every episode", StrategyAll, []*streams.Episode{&e1, &e3, &e4}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			l := newObserveLib(t, Config{UpdateLevel: LevelShow, UpdateStrategy: tc.strategy}, e1, e3, e4)
			l.cache.RecordIntent("alice", obsShow, legacyIntent(1000))

			l.reconcile(t, e3)

			want := map[int]int{}
			for _, ep := range tc.want {
				want[partOf(ep)] = audioOf(ep, "jpn")
			}
			if got := l.audioWritesTo(0); !maps.Equal(got, want) {
				t.Errorf("writes = %v, want %v", got, want)
			}
		})
	}
}

func TestReconcileSecondRunNoReads(t *testing.T) {
	t.Parallel()
	e1, e2 := triAudio(1, 1), triAudio(1, 2)
	l := newObserveLib(t, allShow, e1, e2)
	recordFor(l.cache, obsShow, "jpn", streams.Anchor{}, 1000)
	l.reconcile(t, e1)
	calls, writes := len(l.plx.CallNames()), len(l.plx.Writes())

	l.reconcile(t, e1)

	if got := l.plx.CallNames()[calls:]; !slices.Equal(got, []string{"ShowEpisodes:" + obsShow}) {
		t.Errorf("second replay requests = %v, want only the show's list", got)
	}
	if got := len(l.plx.Writes()); got != writes {
		t.Errorf("second replay wrote %d streams, want none", got-writes)
	}
}
