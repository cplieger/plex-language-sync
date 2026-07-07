package scheduler

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"runtime"
	"strconv"
	"strings"
	"sync"
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

// ---------------------------------------------------------------------------
// deepAnalysisCore — persistence (last-run marker + cache flush)
// ---------------------------------------------------------------------------

// TestDeepAnalysisCore_SetsLastRunAndFlushesCache pins the deferred persistence in
// deepAnalysisCore: it records the last-run marker (the documented cold-restart
// idempotency guard) and flushes the cache exactly once via saveCache. A nil
// saveCache must be a no-op, not a panic.
func TestDeepAnalysisCore_SetsLastRunAndFlushesCache(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{}
	c := fakeapi.NewCache()
	if !c.LastSchedulerRun().IsZero() {
		t.Fatal("precondition: fresh cache must report zero last-run")
	}
	var saveCalls atomic.Int64
	sched := New(
		Config{Enable: true},
		plx, c, &fakeapi.Users{},
		func(_ string) api.PlexReadWriter { return plx },
		&fakeSyncer{},
		func() error { saveCalls.Add(1); return nil },
	)

	sched.deepAnalysisCore(context.Background())

	if c.LastSchedulerRun().IsZero() {
		t.Error("deepAnalysisCore did not record the last-run marker")
	}
	if got := saveCalls.Load(); got != 1 {
		t.Errorf("saveCache called %d times; want 1", got)
	}

	// A nil saveCache must not panic.
	schedNilSave := New(
		Config{Enable: true},
		plx, fakeapi.NewCache(), &fakeapi.Users{},
		func(_ string) api.PlexReadWriter { return plx },
		&fakeSyncer{},
		nil,
	)
	schedNilSave.deepAnalysisCore(context.Background())
}

// ---------------------------------------------------------------------------
// Run — enable gate + initial-run decision (timing-bound loop excluded)
// ---------------------------------------------------------------------------

// TestRun_DisabledReturnsImmediately verifies the Enable=false short-circuit:
// Run must return without touching Plex or the cache.
func TestRun_DisabledReturnsImmediately(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{}
	sched := New(
		Config{Enable: false},
		plx, fakeapi.NewCache(), &fakeapi.Users{},
		func(_ string) api.PlexReadWriter { return plx },
		&fakeSyncer{},
		nil,
	)
	sched.Run(context.Background())
	if plx.Calls.Load() != 0 {
		t.Errorf("Run(disabled) made %d Plex calls; want 0", plx.Calls.Load())
	}
}

// TestRun_RunsInitialAnalysisWhenNeverRun verifies the "run immediately when the
// last-run marker is absent" branch. A pre-cancelled context makes the scheduling
// loop return as soon as the initial pass completes, so the timer wait
// (timing-bound, intentionally untested) is never entered.
func TestRun_RunsInitialAnalysisWhenNeverRun(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{}
	c := fakeapi.NewCache() // zero last-run -> initial pass should fire
	sched := New(
		Config{Enable: true, Interval: 24 * time.Hour},
		plx, c, &fakeapi.Users{},
		func(_ string) api.PlexReadWriter { return plx },
		&fakeSyncer{},
		nil,
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // loop returns on ctx.Done() right after the initial pass
	sched.Run(ctx)

	if c.LastSchedulerRun().IsZero() {
		t.Error("Run did not perform the initial deep analysis (last-run still zero)")
	}
	names := plx.CallNames()
	var sawHistory, sawSections bool
	for _, n := range names {
		if n == "History" {
			sawHistory = true
		}
		if n == "ShowSections" {
			sawSections = true
		}
	}
	if !sawHistory || !sawSections {
		t.Errorf("initial pass did not query Plex (calls=%v); want History and ShowSections", names)
	}
}

// ---------------------------------------------------------------------------
// deep-analysis fetch-error branches — inviolate Loki WARN keys
// ---------------------------------------------------------------------------

// captureSlog redirects the default slog logger to a buffer for the duration of
// fn and returns everything logged, restoring the previous logger afterward.
// Tests using it must not be parallel (the default logger is process-global).
func captureSlog(t *testing.T, fn func()) string {
	t.Helper()
	var buf bytes.Buffer
	prev := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelDebug})))
	t.Cleanup(func() { slog.SetDefault(prev) })
	fn()
	return buf.String()
}

// fetchErrPlex wraps fakeapi.Plex and forces History or ShowSections to fail,
// exercising the two fetch-error branches the base fake (no History/ShowSections
// error fields) cannot reach.
type fetchErrPlex struct {
	*fakeapi.Plex
	historyErr  error
	sectionsErr error
}

func (p *fetchErrPlex) History(ctx context.Context, since int64) ([]plex.HistoryItem, error) {
	if p.historyErr != nil {
		return nil, p.historyErr
	}
	return p.Plex.History(ctx, since)
}

func (p *fetchErrPlex) ShowSections(ctx context.Context) ([]plex.Section, error) {
	if p.sectionsErr != nil {
		return nil, p.sectionsErr
	}
	return p.Plex.ShowSections(ctx)
}

// TestProcessRecentHistory_HistoryFetchErrorWarnsAndAborts pins the inviolate Loki
// WARN key on a history-fetch failure and that no per-item work runs.
func TestProcessRecentHistory_HistoryFetchErrorWarnsAndAborts(t *testing.T) {
	plx := &fetchErrPlex{Plex: &fakeapi.Plex{}, historyErr: errors.New("boom")}
	syncer := &fakeSyncer{}
	sched := New(
		Config{Enable: true},
		plx, fakeapi.NewCache(), &fakeapi.Users{},
		func(_ string) api.PlexReadWriter { return plx.Plex },
		syncer,
		nil,
	)
	out := captureSlog(t, func() {
		sched.processRecentHistory(context.Background(), time.Now().Unix())
	})
	if !strings.Contains(out, "scheduler: failed to fetch history") {
		t.Errorf("missing inviolate WARN key on history-fetch error; log: %q", out)
	}
	if syncer.changeCalls.Load() != 0 {
		t.Errorf("ChangeTracks called %d times after history-fetch error; want 0", syncer.changeCalls.Load())
	}
}

// TestProcessRecentlyAdded_SectionsFetchErrorWarnsAndAborts pins the inviolate Loki
// WARN key on a sections-fetch failure and that no episodes are processed.
func TestProcessRecentlyAdded_SectionsFetchErrorWarnsAndAborts(t *testing.T) {
	plx := &fetchErrPlex{Plex: &fakeapi.Plex{}, sectionsErr: errors.New("boom")}
	syncer := &fakeSyncer{}
	sched := New(
		Config{Enable: true},
		plx, fakeapi.NewCache(), &fakeapi.Users{},
		func(_ string) api.PlexReadWriter { return plx.Plex },
		syncer,
		nil,
	)
	out := captureSlog(t, func() {
		sched.processRecentlyAdded(context.Background(), time.Now().Unix())
	})
	if !strings.Contains(out, "scheduler: failed to fetch sections") {
		t.Errorf("missing inviolate WARN key on sections-fetch error; log: %q", out)
	}
	if syncer.processCalls.Load() != 0 {
		t.Errorf("ProcessNewOrUpdated called %d times after sections-fetch error; want 0", syncer.processCalls.Load())
	}
}

func TestProcessRecentHistory_BreakerAbortLogsInviolateWarn(t *testing.T) {
	items := make([]plex.HistoryItem, 100)
	for i := range items {
		items[i] = plex.HistoryItem{RatingKey: "ep", Type: "episode"}
	}
	plx := &fakeapi.Plex{HistoryItems: items, EpisodeErr: errors.New("fetch failed")}
	sched := New(
		Config{Enable: true},
		plx, fakeapi.NewCache(), &fakeapi.Users{},
		func(_ string) api.PlexReadWriter { return plx },
		&fakeSyncer{},
		nil,
	)

	out := captureSlog(t, func() {
		sched.processRecentHistory(context.Background(), time.Now().Unix())
	})

	if !strings.Contains(out, "scheduler: aborting history processing after consecutive failures") {
		t.Errorf("missing inviolate breaker-abort WARN key; log: %q", out)
	}
	if !strings.Contains(out, "consecutive_errors=") {
		t.Errorf("breaker-abort WARN must carry the consecutive_errors attribute; log: %q", out)
	}
}

// TestProcessRecentlyAddedEpisode_GenericFetchErrorSkipsSync pins the
// non-ErrNotFound fetch-error contract: a failed Episode() fetch during the
// recently-added sweep must Debug-log the failure and must NOT call the
// Syncer (a fetch failure must never propagate to a sync write). Not parallel:
// captureSlog mutates the process-global default logger.
func TestProcessRecentlyAddedEpisode_GenericFetchErrorSkipsSync(t *testing.T) {
	plx := &fakeapi.Plex{EpisodeErr: errors.New("boom")}
	c := fakeapi.NewCache()
	syncer := &fakeSyncer{}
	sched := New(
		Config{Enable: true},
		plx, c, &fakeapi.Users{},
		func(_ string) api.PlexReadWriter { return plx },
		syncer,
		nil,
	)
	ep := &streams.Episode{RatingKey: "100"}
	out := captureSlog(t, func() {
		sched.processRecentlyAddedEpisode(context.Background(), ep)
	})
	if syncer.processCalls.Load() != 0 {
		t.Errorf("ProcessNewOrUpdated called %d times after fetch error; want 0", syncer.processCalls.Load())
	}
	if !strings.Contains(out, "scheduler: failed to fetch recently added episode") {
		t.Errorf("non-ErrNotFound fetch error must be logged at debug; log: %q", out)
	}
}

// TestProcessRecentlyAddedEpisode_NotFoundSkipsSyncSilently pins the
// ErrNotFound contract: a not-found episode during the recently-added sweep
// is a SILENT skip (no fetch-failure debug line) and must NOT call the Syncer.
// The errors.Is(ErrNotFound) guard exists to suppress log-spam on the common
// "episode vanished mid-sweep" race. Not parallel: captureSlog mutates the
// process-global default logger.
func TestProcessRecentlyAddedEpisode_NotFoundSkipsSyncSilently(t *testing.T) {
	plx := &fakeapi.Plex{} // no EpisodeByKey entry -> Episode returns plex.ErrNotFound
	c := fakeapi.NewCache()
	syncer := &fakeSyncer{}
	sched := New(
		Config{Enable: true},
		plx, c, &fakeapi.Users{},
		func(_ string) api.PlexReadWriter { return plx },
		syncer,
		nil,
	)
	ep := &streams.Episode{RatingKey: "404"}
	out := captureSlog(t, func() {
		sched.processRecentlyAddedEpisode(context.Background(), ep)
	})
	if syncer.processCalls.Load() != 0 {
		t.Errorf("ProcessNewOrUpdated called %d times after ErrNotFound; want 0", syncer.processCalls.Load())
	}
	if strings.Contains(out, "scheduler: failed to fetch recently added episode") {
		t.Errorf("ErrNotFound must be a silent skip, but a fetch-failure debug line was logged; log: %q", out)
	}
}

// blockingDeepAnalysisPlex wraps fakeapi.Plex and makes the first History call
// block until released, signalling once it has been entered. It is the seam that
// lets a test hold the singleflight winner inside deepAnalysisCore long enough for
// a second caller to collapse into the loser branch deterministically (mirrors the
// fetchErrPlex / recentlyAddedErrPlex wrappers). entered is closed on the first
// History call; release gates that call's return so the winner stays in-flight
// until the test allows it. calls counts History invocations: the singleflight
// winner is the only caller that reaches History, so a post-test value of 1 proves
// the second caller collapsed rather than running a second deep-analysis pass.
type blockingDeepAnalysisPlex struct {
	*fakeapi.Plex
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64
	once    sync.Once
}

func (p *blockingDeepAnalysisPlex) History(ctx context.Context, since int64) ([]plex.HistoryItem, error) {
	p.calls.Add(1)
	p.once.Do(func() { close(p.entered) })
	<-p.release
	return p.Plex.History(ctx, since)
}

// TestDeepAnalysis_ConcurrentCallCollapsesAndWarnsOnce pins the singleflight
// collapse path of deepAnalysis: when two Run goroutines trigger an overlapping
// deep-analysis tick, exactly one (the winner) executes deepAnalysisCore and the
// other (the loser) collapses into the in-flight call, logging the inviolate Loki
// WARN key "scheduler: deep analysis already in progress, skipping" EXACTLY ONCE.
//
// Ordering is what makes the collapse deterministic rather than schedule-
// dependent: the winner is held inside deepAnalysisCore (blocked on a History
// fetch) so it owns the "deep_analysis" singleflight key; only then is the loser
// started. singleflight.Do blocks the loser on the winner's in-flight call until
// the winner's callback returns, so releasing the winner AFTER the loser has
// joined guarantees the loser observes shared==true and logs the skip exactly
// once. The History call-count oracle (want 1) independently proves the collapse:
// had the loser failed to join, it would have run a second pass and History would
// have been called twice. Not parallel: captureSlog mutates the process-global
// default logger.
func TestDeepAnalysis_ConcurrentCallCollapsesAndWarnsOnce(t *testing.T) {
	plx := &blockingDeepAnalysisPlex{
		Plex:    &fakeapi.Plex{},
		entered: make(chan struct{}),
		release: make(chan struct{}),
	}
	sched := New(
		Config{Enable: true},
		plx, fakeapi.NewCache(), &fakeapi.Users{},
		func(_ string) api.PlexReadWriter { return plx.Plex },
		&fakeSyncer{},
		nil,
	)

	out := captureSlog(t, func() {
		var wg sync.WaitGroup
		wg.Go(func() {
			sched.deepAnalysis(context.Background()) // winner: blocks inside History
		})

		// Wait until the winner is inside the singleflight callback (blocked
		// in History), so it owns the "deep_analysis" key before the loser
		// calls Do.
		<-plx.entered

		loserJoined := make(chan struct{})
		wg.Go(func() {
			close(loserJoined)                       // about to enter Do; hand the scheduler over below
			sched.deepAnalysis(context.Background()) // loser: collapses, logs skip
		})

		// Let the loser goroutine run far enough to register as a duplicate
		// inside singleflight.Do (which then parks on the winner's in-flight
		// call) before the winner is released. The History call-count assertion
		// below is the backstop: if the loser had not yet joined when the
		// winner finished, History would be hit twice and the test fails.
		<-loserJoined
		runtime.Gosched()

		close(plx.release) // winner finishes -> singleflight wakes the loser
		wg.Wait()
	})

	const skipKey = "scheduler: deep analysis already in progress, skipping"
	if got := strings.Count(out, skipKey); got != 1 {
		t.Errorf("skip WARN appeared %d times; want exactly 1 (one winner, one loser); log: %q", got, out)
	}
	// Oracle: only the singleflight winner reaches History. Exactly one call
	// proves the loser collapsed into the in-flight run instead of executing a
	// second deep-analysis pass.
	if got := plx.calls.Load(); got != 1 {
		t.Errorf("History called %d times; want 1 (loser must collapse, not run a second pass)", got)
	}
}

// recentlyAddedErrPlex wraps fakeapi.Plex and forces RecentlyAdded to fail for
// a chosen set of section keys, leaving the rest to the base fake. The base
// fake's RecentlyAdded never errors, so this wrapper is the only seam that
// reaches feedRecentlyAdded's partial-failure accounting (mirrors the existing
// fetchErrPlex wrapper used for History/ShowSections).
type recentlyAddedErrPlex struct {
	*fakeapi.Plex
	failSections map[string]bool
}

func (p *recentlyAddedErrPlex) RecentlyAdded(ctx context.Context, sectionKey plex.RatingKey, since int64) ([]streams.Episode, error) {
	if p.failSections[sectionKey.String()] {
		return nil, errors.New("section fetch boom")
	}
	return p.Plex.RecentlyAdded(ctx, sectionKey, since)
}

// TestFeedRecentlyAdded_PartialSectionFailureWarnsWithCounts pins the aggregate
// sweep-incomplete WARN: when some (but not all) sections fail their
// RecentlyAdded fetch, the sweep finishes processing the sections that
// succeeded AND emits one WARN reporting failed/total section counts.
// Not parallel: captureSlog mutates the process-global default logger.
func TestFeedRecentlyAdded_PartialSectionFailureWarnsWithCounts(t *testing.T) {
	base := &fakeapi.Plex{
		Sections: []plex.Section{
			{Key: "1", Title: "TV"},
			{Key: "2", Title: "Anime"},
		},
		RecentlyAddedBySec: map[string][]streams.Episode{
			"2": {{RatingKey: "201"}},
		},
		EpisodeByKey: map[string]*streams.Episode{
			"201": {RatingKey: "201"},
		},
	}
	plx := &recentlyAddedErrPlex{Plex: base, failSections: map[string]bool{"1": true}}
	syncer := &fakeSyncer{}
	sched := New(
		Config{Enable: true},
		plx, fakeapi.NewCache(), &fakeapi.Users{},
		func(_ string) api.PlexReadWriter { return plx.Plex },
		syncer,
		nil,
	)
	out := captureSlog(t, func() {
		sched.processRecentlyAdded(context.Background(), time.Now().Unix())
	})
	if !strings.Contains(out, "scheduler: recently-added sweep incomplete, some sections failed to fetch") {
		t.Errorf("missing aggregate sweep-incomplete WARN on partial section failure; log: %q", out)
	}
	if !strings.Contains(out, "failed_sections=1") {
		t.Errorf("aggregate WARN must report failed_sections=1; log: %q", out)
	}
	if !strings.Contains(out, "total_sections=2") {
		t.Errorf("aggregate WARN must report total_sections=2; log: %q", out)
	}
	if syncer.processCalls.Load() != 1 {
		t.Errorf("ProcessNewOrUpdated called %d times; want 1 (only the succeeding section's episode)", syncer.processCalls.Load())
	}
}

// TestProcessHistoryItem_NilPerUserClientSkips pins the fail-closed per-user
// write contract: when the per-user client is nil, the history item is skipped
// (no admin-client fallback, no Episode fetch, no ChangeTracksForEpisode) and
// the skip is logged. Not parallel: captureSlog mutates the global logger.
func TestProcessHistoryItem_NilPerUserClientSkips(t *testing.T) {
	plx := &fakeapi.Plex{
		HistoryItems: []plex.HistoryItem{{RatingKey: "300", Type: "episode"}},
	}
	syncer := &fakeSyncer{}
	sched := New(
		Config{Enable: true},
		plx, fakeapi.NewCache(), &fakeapi.Users{},
		func(_ string) api.PlexReadWriter { return nil },
		syncer,
		nil,
	)
	out := captureSlog(t, func() {
		sched.processRecentHistory(context.Background(), time.Now().Unix())
	})
	if syncer.changeCalls.Load() != 0 {
		t.Errorf("ChangeTracks called %d times with a nil per-user client; want 0 (must skip, never fall back to admin)", syncer.changeCalls.Load())
	}
	if plx.Calls.Load() != 1 {
		t.Errorf("Plex called %d times; want 1 (History only, no per-user Episode fetch when the client is nil)", plx.Calls.Load())
	}
	if !strings.Contains(out, "scheduler: no per-user client, skipping history item") {
		t.Errorf("missing per-user-client skip WARN; log: %q", out)
	}
}

// TestDeepAnalysisCore_SaveCacheErrorWarns pins the deferred cache-flush error
// branch: a failing saveCache still records the last-run marker and logs the
// "cache save failed" WARN rather than swallowing the error. Companion to
// TestDeepAnalysisCore_SetsLastRunAndFlushesCache. Not parallel: captureSlog
// mutates the global logger.
func TestDeepAnalysisCore_SaveCacheErrorWarns(t *testing.T) {
	plx := &fakeapi.Plex{}
	c := fakeapi.NewCache()
	sched := New(
		Config{Enable: true},
		plx, c, &fakeapi.Users{},
		func(_ string) api.PlexReadWriter { return plx },
		&fakeSyncer{},
		func() error { return errors.New("disk full") },
	)
	out := captureSlog(t, func() {
		sched.deepAnalysisCore(context.Background())
	})
	if !strings.Contains(out, "cache save failed") {
		t.Errorf("missing cache-save-failure WARN when saveCache errors; log: %q", out)
	}
	if c.LastSchedulerRun().IsZero() {
		t.Error("last-run marker must still be recorded even when the cache flush fails")
	}
}

// TestScheduler_CancelledContextSkipsPerItemWork pins graceful-shutdown
// responsiveness: a cancelled context short-circuits the deep-analysis feeders
// so no per-item work runs (regression guard for the ctx checks in feedHistory
// and feedRecentlyAdded).
func TestScheduler_CancelledContextSkipsPerItemWork(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	t.Run("history feeder", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{
			HistoryItems: []plex.HistoryItem{{RatingKey: "1", Type: "episode"}, {RatingKey: "2", Type: "episode"}},
			EpisodeByKey: map[string]*streams.Episode{"1": {RatingKey: "1"}, "2": {RatingKey: "2"}},
		}
		syncer := &fakeSyncer{}
		sched := New(Config{Enable: true}, plx, fakeapi.NewCache(), &fakeapi.Users{},
			func(_ string) api.PlexReadWriter { return plx }, syncer, nil)
		sched.processRecentHistory(ctx, time.Now().Unix())
		if syncer.changeCalls.Load() != 0 {
			t.Errorf("ChangeTracks called %d times under a cancelled context; want 0", syncer.changeCalls.Load())
		}
	})
	t.Run("recently-added feeder", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{
			Sections:           []plex.Section{{Key: "1", Title: "TV"}},
			RecentlyAddedBySec: map[string][]streams.Episode{"1": {{RatingKey: "101"}}},
			EpisodeByKey:       map[string]*streams.Episode{"101": {RatingKey: "101"}},
		}
		syncer := &fakeSyncer{}
		sched := New(Config{Enable: true}, plx, fakeapi.NewCache(), &fakeapi.Users{},
			func(_ string) api.PlexReadWriter { return plx }, syncer, nil)
		sched.processRecentlyAdded(ctx, time.Now().Unix())
		if syncer.processCalls.Load() != 0 {
			t.Errorf("ProcessNewOrUpdated called %d times under a cancelled context; want 0", syncer.processCalls.Load())
		}
	})
}
