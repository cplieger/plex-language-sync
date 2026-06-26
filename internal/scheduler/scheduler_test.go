package scheduler

import (
	"context"
	"errors"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/plex-language-sync/internal/api"
	"github.com/cplieger/plex-language-sync/internal/ignore"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plex-language-sync/internal/testsupport/fakeapi"
)

// ---------------------------------------------------------------------------
// nextScheduledRun
// ---------------------------------------------------------------------------

func TestNextScheduledRun(t *testing.T) {
	t.Parallel()
	tests := []struct {
		now    time.Time
		want   time.Time
		name   string
		hour   int
		minute int
	}{
		{
			name:   "future today",
			now:    time.Date(2026, 3, 15, 10, 0, 0, 0, time.UTC),
			hour:   14,
			minute: 30,
			want:   time.Date(2026, 3, 15, 14, 30, 0, 0, time.UTC),
		},
		{
			name:   "rolls to tomorrow when passed",
			now:    time.Date(2026, 3, 15, 15, 0, 0, 0, time.UTC),
			hour:   14,
			minute: 30,
			want:   time.Date(2026, 3, 16, 14, 30, 0, 0, time.UTC),
		},
		{
			name:   "exact boundary rolls forward",
			now:    time.Date(2026, 3, 15, 14, 30, 0, 0, time.UTC),
			hour:   14,
			minute: 30,
			want:   time.Date(2026, 3, 16, 14, 30, 0, 0, time.UTC),
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			t.Parallel()
			got := nextScheduledRun(tc.now, tc.hour, tc.minute)
			if !got.Equal(tc.want) {
				t.Errorf("nextScheduledRun = %v, want %v", got, tc.want)
			}
		})
	}
}

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

type fakeSyncer struct {
	changeCalls  atomic.Int64
	processCalls atomic.Int64
}

func (s *fakeSyncer) ChangeTracksForEpisode(_ context.Context, _ api.PlexReadWriter, _ string, _ *streams.Episode, _ string) {
	s.changeCalls.Add(1)
}

func (s *fakeSyncer) ProcessNewOrUpdatedEpisodeAllUsers(_ context.Context, _ *streams.Episode, _ string) {
	s.processCalls.Add(1)
}

// fakeIgnore is an api.IgnoreChecker that returns fixed decisions.
// ShouldSkipEpisode defaults to false; set skipEpisode to flip it.
// Libraries holds the set of library titles to skip.
type fakeIgnore struct {
	libraries   map[string]bool
	skipEpisode bool
}

func (f *fakeIgnore) IgnoreLibrary(title string) bool {
	if f == nil {
		return false
	}
	return f.libraries[title]
}

func (f *fakeIgnore) ShouldSkipEpisode(_ context.Context, _ api.PlexReader, _ *streams.Episode) bool {
	if f == nil {
		return false
	}
	return f.skipEpisode
}

var _ api.IgnoreChecker = (*fakeIgnore)(nil)

// ---------------------------------------------------------------------------
// processRecentHistory — circuit breaker
// ---------------------------------------------------------------------------

func TestProcessRecentHistory_CircuitBreakerAbortsAtThreshold(t *testing.T) {
	t.Parallel()
	// Every Episode() call returns an error. With maxConsecutiveErrors=5
	// and deepAnalysisConcurrency=4 workers, once the breaker trips the
	// remaining items drain without calling the Syncer. Because multiple
	// workers race to increment the counter, we give some slack on the
	// exact number of Episode calls but assert that NO ChangeTracks call
	// ever happened (every fetch fails) AND that we stopped well short
	// of the full list size.
	items := make([]plex.HistoryItem, 100)
	for i := range items {
		items[i] = plex.HistoryItem{
			RatingKey: "ep", Type: "episode",
		}
	}
	plx := &fakeapi.Plex{
		HistoryItems: items,
		EpisodeErr:   errors.New("fetch failed"),
	}
	c := fakeapi.NewCache()
	syncer := &fakeSyncer{}
	sched := New(
		Config{Enable: true},
		plx, c, &fakeapi.Users{},
		func(_ string) api.PlexReadWriter { return plx },
		syncer,
		nil,
	)

	sched.processRecentHistory(context.Background(), time.Now().Unix())

	if syncer.changeCalls.Load() != 0 {
		t.Errorf("ChangeTracks called %d times on all-fail history; want 0", syncer.changeCalls.Load())
	}
	// Breaker trips at maxConsecutiveErrors with deepAnalysisConcurrency
	// workers draining remaining items. The total Episode() calls are
	// bounded by (breaker threshold + in-flight workers) but can drift
	// slightly under concurrency; assert a generous upper bound well
	// below the full 100.
	calls := plx.Calls.Load()
	if calls > int64(maxConsecutiveErrors+2*deepAnalysisConcurrency) {
		t.Errorf("Episode called %d times; circuit breaker should have aborted earlier (threshold ~= %d + %d)",
			calls, maxConsecutiveErrors, deepAnalysisConcurrency)
	}
	if calls == 100 {
		t.Errorf("Episode called %d times (full list); breaker did not trip", calls)
	}
}

func TestProcessRecentHistory_SuccessResetsBreaker(t *testing.T) {
	t.Parallel()
	// Strictly alternating success/failure items: every failure is
	// immediately followed by a success that resets the consecutive-
	// error counter, so the breaker must never trip and all items are
	// processed.
	//
	// The pool is pinned to a single worker below so processing is
	// serial and the assertion is exact. With the default 4-worker pool
	// the shared atomic counter has no well-defined "consecutive" order:
	// a burst of failures can transiently spike it to the threshold
	// before an interleaved success resets it, tripping the breaker
	// nondeterministically under -race. The concurrent trip path is
	// covered by TestProcessRecentHistory_CircuitBreakerAbortsAtThreshold;
	// here we isolate and deterministically verify the reset semantics.
	items := make([]plex.HistoryItem, 50)
	episodeByKey := make(map[string]*streams.Episode, len(items))
	for i := range items {
		key := "ep" + strconv.Itoa(i)
		items[i] = plex.HistoryItem{RatingKey: key, Type: "episode"}
		if i%2 == 0 { // every other item resolves
			episodeByKey[key] = &streams.Episode{RatingKey: key}
		}
	}
	plx := &fakeapi.Plex{HistoryItems: items, EpisodeByKey: episodeByKey}
	c := fakeapi.NewCache()
	syncer := &fakeSyncer{}
	sched := New(
		Config{Enable: true},
		plx, c, &fakeapi.Users{},
		func(_ string) api.PlexReadWriter { return plx },
		syncer,
		nil,
	)
	sched.workers = 1 // serial run -> deterministic breaker-reset semantics

	sched.processRecentHistory(context.Background(), time.Now().Unix())

	// plx.Calls counts every fake Plex method call: the single History
	// fetch at the top of processRecentHistory plus one Episode fetch
	// per item. All items must be fetched — the breaker never trips.
	if got, want := plx.Calls.Load(), int64(len(items)+1); got != want {
		t.Errorf("Plex called %d times; want %d (1 History + %d Episode fetches; breaker must not trip)", got, want, len(items))
	}
	if got, want := syncer.changeCalls.Load(), int64(len(episodeByKey)); got != want {
		t.Errorf("ChangeTracks called %d times; want %d (one per resolved episode)", got, want)
	}
}

// ---------------------------------------------------------------------------
// processRecentlyAddedEpisode — dedup + skip
// ---------------------------------------------------------------------------

func TestProcessRecentlyAddedEpisode_DedupSkipsProcessed(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{}
	c := fakeapi.NewCache()
	c.MarkProcessed("scheduler:100")
	syncer := &fakeSyncer{}
	sched := New(
		Config{Enable: true},
		plx, c, &fakeapi.Users{},
		func(_ string) api.PlexReadWriter { return plx },
		syncer,
		nil,
	)
	ep := &streams.Episode{RatingKey: "100"}
	sched.processRecentlyAddedEpisode(context.Background(), ep)
	if plx.Calls.Load() != 0 {
		t.Errorf("Episode called %d times on deduped key; want 0", plx.Calls.Load())
	}
	if syncer.processCalls.Load() != 0 {
		t.Errorf("ProcessNewOrUpdated called %d times on deduped key; want 0", syncer.processCalls.Load())
	}
}

func TestProcessRecentlyAddedEpisode_SkipsIgnoredShow(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		EpisodeByKey: map[string]*streams.Episode{
			"100": {RatingKey: "100", GrandparentRatingKey: "42"},
		},
	}
	c := fakeapi.NewCache()
	syncer := &fakeSyncer{}
	sched := New(
		Config{Enable: true, Ignore: &fakeIgnore{skipEpisode: true}},
		plx, c, &fakeapi.Users{},
		func(_ string) api.PlexReadWriter { return plx },
		syncer,
		nil,
	)
	ep := &streams.Episode{RatingKey: "100", GrandparentRatingKey: "42"}
	sched.processRecentlyAddedEpisode(context.Background(), ep)
	if syncer.processCalls.Load() != 0 {
		t.Errorf("ProcessNewOrUpdated called when show should have been skipped")
	}
}

func TestProcessRecentlyAddedEpisode_HappyPathDelegates(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		EpisodeByKey: map[string]*streams.Episode{
			"100": {RatingKey: "100", GrandparentRatingKey: "42"},
		},
	}
	c := fakeapi.NewCache()
	syncer := &fakeSyncer{}
	sched := New(
		Config{Enable: true, Ignore: &fakeIgnore{skipEpisode: false}},
		plx, c, &fakeapi.Users{},
		func(_ string) api.PlexReadWriter { return plx },
		syncer,
		nil,
	)
	ep := &streams.Episode{RatingKey: "100", GrandparentRatingKey: "42"}
	sched.processRecentlyAddedEpisode(context.Background(), ep)
	if syncer.processCalls.Load() != 1 {
		t.Errorf("ProcessNewOrUpdated called %d times; want 1", syncer.processCalls.Load())
	}
}

// ---------------------------------------------------------------------------
// processRecentlyAdded — worker pool distributes sections' episodes
// ---------------------------------------------------------------------------

func TestProcessRecentlyAdded_FansOutAcrossSections(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		Sections: []plex.Section{
			{Key: "1", Title: "TV"},
			{Key: "2", Title: "Anime"},
		},
		RecentlyAddedBySec: map[string][]streams.Episode{
			"1": {{RatingKey: "101"}, {RatingKey: "102"}},
			"2": {{RatingKey: "201"}},
		},
		EpisodeByKey: map[string]*streams.Episode{
			"101": {RatingKey: "101"},
			"102": {RatingKey: "102"},
			"201": {RatingKey: "201"},
		},
	}
	c := fakeapi.NewCache()
	syncer := &fakeSyncer{}
	sched := New(
		Config{Enable: true},
		plx, c, &fakeapi.Users{},
		func(_ string) api.PlexReadWriter { return plx },
		syncer,
		nil,
	)
	sched.processRecentlyAdded(context.Background(), time.Now().Unix())
	if syncer.processCalls.Load() != 3 {
		t.Errorf("ProcessNewOrUpdated called %d times; want 3 (one per episode)", syncer.processCalls.Load())
	}
}

func TestProcessRecentlyAdded_HonorsIgnoreLibraries(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		Sections: []plex.Section{
			{Key: "1", Title: "Kids"},
			{Key: "2", Title: "TV"},
		},
		RecentlyAddedBySec: map[string][]streams.Episode{
			"1": {{RatingKey: "101"}}, // ignored
			"2": {{RatingKey: "201"}},
		},
		EpisodeByKey: map[string]*streams.Episode{
			"201": {RatingKey: "201"},
		},
	}
	c := fakeapi.NewCache()
	syncer := &fakeSyncer{}
	sched := New(
		Config{Enable: true, Ignore: ignore.NewPolicy([]string{"Kids"}, nil)},
		plx, c, &fakeapi.Users{},
		func(_ string) api.PlexReadWriter { return plx },
		syncer,
		nil,
	)
	sched.processRecentlyAdded(context.Background(), time.Now().Unix())
	if syncer.processCalls.Load() != 1 {
		t.Errorf("ProcessNewOrUpdated called %d times; want 1 (Kids section ignored)", syncer.processCalls.Load())
	}
}
