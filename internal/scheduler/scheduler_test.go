package scheduler

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"plex-language-sync/internal/api"
	"plex-language-sync/internal/ignore"
	"plex-language-sync/internal/plex"
	"plex-language-sync/internal/streams"
	"plex-language-sync/internal/testsupport/fakeapi"
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
	// A mix of successes and failures with success rate > 1/5 keeps the
	// breaker from tripping: each success resets the counter before it
	// reaches maxConsecutiveErrors.
	items := make([]plex.HistoryItem, 50)
	episodeByKey := make(map[string]*streams.Episode, len(items))
	for i := range items {
		key := "ep" + itoa(i)
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

	sched.processRecentHistory(context.Background(), time.Now().Unix())

	// Assertion shape is intentionally loose to stay stable under `-race`
	// goroutine scheduling. With deepAnalysisConcurrency=4 workers and a
	// shared atomic error counter, a brief run of ≥ maxConsecutiveErrors
	// failures landing before any success resets the counter can still
	// trip the breaker early under heavy race scheduling, even though
	// the long-run success rate (50%) is well above 1/5.
	//
	// The semantic we actually care about: successes DO reset the
	// counter — at least some work completes AND the counter was reset
	// enough times that the test exceeds the worst-case trip-at-5-fails
	// bound we assert in TestProcessRecentHistory_TripsOnConsecutive
	// Failures. That bound is maxConsecutiveErrors + 2 *
	// deepAnalysisConcurrency, so assertions here check we cleared it.
	calls := plx.Calls.Load()
	resetFloor := int64(maxConsecutiveErrors + 2*deepAnalysisConcurrency)
	if calls <= resetFloor {
		t.Errorf("Episode called %d times; expected >%d (breaker should reset on successes, not trip like in TripsOnConsecutiveFailures)",
			calls, resetFloor)
	}
	if syncer.changeCalls.Load() == 0 {
		t.Error("ChangeTracks never called; expected at least one success")
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

// itoa is a tiny inline formatter to avoid a strconv import in a test-
// only file.
func itoa(n int) string {
	if n == 0 {
		return "0"
	}
	sign := ""
	if n < 0 {
		sign = "-"
		n = -n
	}
	var buf [20]byte
	i := len(buf)
	for n > 0 {
		i--
		buf[i] = byte('0' + n%10)
		n /= 10
	}
	return sign + string(buf[i:])
}
