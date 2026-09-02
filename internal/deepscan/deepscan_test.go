package deepscan

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cplieger/plex-language-sync/internal/ignore"
	"github.com/cplieger/plex-language-sync/internal/plex"
	"github.com/cplieger/plex-language-sync/internal/streams"
	"github.com/cplieger/plex-language-sync/internal/testsupport/fakeapi"
	"github.com/cplieger/scheduler/v4"
)

// ---------------------------------------------------------------------------
// Fakes
// ---------------------------------------------------------------------------

// testStamp returns a Stamp backed by a fresh temp-dir file with no record:
// the "never run" state.
func testStamp(t *testing.T) *scheduler.Stamp {
	t.Helper()
	return scheduler.NewStamp(filepath.Join(t.TempDir(), "last-run"))
}

// stampAt returns a Stamp seeded with a completed run dated at. Record always
// stamps the current time, so backdating writes the file format directly.
func stampAt(t *testing.T, at time.Time) *scheduler.Stamp {
	t.Helper()
	path := filepath.Join(t.TempDir(), "last-run")
	line := at.UTC().Format(time.RFC3339Nano) + " ok\n"
	if err := os.WriteFile(path, []byte(line), 0o600); err != nil {
		t.Fatalf("seed stamp: %v", err)
	}
	return scheduler.NewStamp(path)
}

// stampTime returns the recorded run time, or the zero time when no run was
// ever recorded — the same collapse the production look-back read uses.
func stampTime(t *testing.T, st *scheduler.Stamp) time.Time {
	t.Helper()
	rec, _ := st.Last()
	return rec.Time
}

type fakeSyncer struct {
	changeCalls  atomic.Int64
	processCalls atomic.Int64
}

func (s *fakeSyncer) ReconcileWithIntent(_ context.Context, _ string, _ *streams.Episode, _ int64, _ string) {
	s.changeCalls.Add(1)
}

func (s *fakeSyncer) ProcessNewOrUpdatedEpisodeAllUsers(_ context.Context, _ *streams.Episode, _ string) {
	s.processCalls.Add(1)
}

// fakeIgnore is a skipChecker that returns fixed decisions.
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

func (f *fakeIgnore) ShouldSkipEpisode(_ context.Context, _ *streams.Episode) bool {
	if f == nil {
		return false
	}
	return f.skipEpisode
}

var _ skipChecker = (*fakeIgnore)(nil)

// ---------------------------------------------------------------------------
// worker pool sizing
// ---------------------------------------------------------------------------

// TestWorkerCount pins the pool-size seam the deterministic tests in this file
// rest on: an unset pool means the production concurrency, and an explicit pool
// means exactly that many workers. A fallback that also swallowed 1 would turn
// every "workers = 1" test here into a concurrent run, where the shared
// consecutive-error counter has no well-defined order — the tests would still
// pass, and would stop testing what they say they test.
func TestWorkerCount(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name    string
		workers int
		want    int
	}{
		{"unset falls back to the production pool", 0, deepAnalysisConcurrency},
		{"one means serial", 1, 1},
		{"explicit pool is honoured", 3, 3},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			s := &Scheduler{workers: tt.workers}
			if got := s.workerCount(); got != tt.want {
				t.Errorf("workerCount() with workers=%d = %d, want %d", tt.workers, got, tt.want)
			}
		})
	}
}

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
		Deps{
			Plex:       plx,
			Cache:      c,
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       syncer,
			SaveCache:  nil,
		},
	)

	sched.processRecentHistory(t.Context(), time.Now().Unix())

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
		Deps{
			Plex:       plx,
			Cache:      c,
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       syncer,
			SaveCache:  nil,
		},
	)
	sched.workers = 1 // serial run -> deterministic breaker-reset semantics

	sched.processRecentHistory(t.Context(), time.Now().Unix())

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

// TestHistoryWorker_TrippedBreakerDrainsWithoutFetching pins the worker's half
// of the circuit breaker. Once the shared counter has REACHED the threshold, a
// worker still holding queued items empties the queue without doing any work,
// which is what lets a feeder blocked on a send finish and close the channel.
// Driving the worker directly is what makes the threshold exact: in a full pass
// the feeder carries the same guard on the same counter, so which side of the
// boundary a queued item lands on is a race.
func TestHistoryWorker_TrippedBreakerDrainsWithoutFetching(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{EpisodeErr: errors.New("fetch failed")}
	sched := New(
		Config{Enable: true},
		Deps{
			Plex:       plx,
			Cache:      fakeapi.NewCache(),
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       &fakeSyncer{},
			SaveCache:  nil,
		},
	)

	work := make(chan plex.HistoryItem, 2)
	work <- plex.HistoryItem{AccountID: 1, RatingKey: "1", Type: plex.TypeEpisode}
	work <- plex.HistoryItem{AccountID: 1, RatingKey: "2", Type: plex.TypeEpisode}
	close(work)

	var consecutiveErrors, totalErrors atomic.Int32
	consecutiveErrors.Store(maxConsecutiveErrors) // the breaker has just tripped
	var unknownUsers sync.Map

	sched.historyWorker(t.Context(), work, &consecutiveErrors, &totalErrors, &unknownUsers)

	if got := plx.Calls.Load(); got != 0 {
		t.Errorf("historyWorker made %d Plex calls with the counter at the %d-error threshold; want 0 (drain, do not work)",
			got, maxConsecutiveErrors)
	}
	if got := len(work); got != 0 {
		t.Errorf("historyWorker left %d items queued; want 0 (the drain is what releases the feeder)", got)
	}
	if got := totalErrors.Load(); got != 0 {
		t.Errorf("totalErrors = %d after a drain-only pass; want 0 (no fetch was attempted, so nothing failed)", got)
	}
}

// ---------------------------------------------------------------------------
// processRecentlyAddedEpisode — dedup + skip
// ---------------------------------------------------------------------------

func TestProcessRecentlyAddedEpisode_DedupSkipsProcessed(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		EpisodeByKey: map[string]*streams.Episode{
			"100": {RatingKey: "100", GrandparentRatingKey: "42"},
		},
	}
	c := fakeapi.NewCache()
	c.MarkProcessed("scheduler:100")
	syncer := &fakeSyncer{}
	sched := New(
		Config{Enable: true},
		Deps{
			Plex:       plx,
			Cache:      c,
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       syncer,
			SaveCache:  nil,
		},
	)
	ep := &streams.Episode{RatingKey: "100"}
	sched.processRecentlyAddedEpisode(t.Context(), ep)
	// Dedup marks on success (after the fetch), so a fetch DOES occur on an
	// already-deduped key (a redundant idempotent read); the guarantee is
	// "not PROCESSED", not "not fetched". The CheckAndMark guard still skips
	// the per-user processing.
	if syncer.processCalls.Load() != 0 {
		t.Errorf("ProcessNewOrUpdated called %d times on deduped key; want 0", syncer.processCalls.Load())
	}
}

// A transient (non-NotFound) fetch failure must NOT mark the dedup key, so a
// later pass retries instead of suppressing the episode for the dedup window.
// Pre-fix the key was marked before the fetch, permanently skipping a
// transiently-failing episode until the window expired.
func TestProcessRecentlyAddedEpisode_TransientFetchFailureRetries(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{
		EpisodeErr: errors.New("plex 500"),
		EpisodeByKey: map[string]*streams.Episode{
			"100": {RatingKey: "100", GrandparentRatingKey: "42"},
		},
	}
	c := fakeapi.NewCache()
	syncer := &fakeSyncer{}
	sched := New(
		Config{Enable: true},
		Deps{
			Plex:       plx,
			Cache:      c,
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       syncer,
			SaveCache:  nil,
		},
	)
	ep := &streams.Episode{RatingKey: "100"}

	// First pass: fetch fails transiently → not processed, key left unmarked.
	sched.processRecentlyAddedEpisode(t.Context(), ep)
	if syncer.processCalls.Load() != 0 {
		t.Fatalf("processed despite a fetch failure; want 0")
	}

	// Recover: the key was never marked, so the retry processes the episode.
	plx.EpisodeErr = nil
	sched.processRecentlyAddedEpisode(t.Context(), ep)
	if got := syncer.processCalls.Load(); got != 1 {
		t.Errorf("retry after transient failure processed %d times; want 1 (key must not have been marked on failure)", got)
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
		Deps{
			Plex:       plx,
			Cache:      c,
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       syncer,
			SaveCache:  nil,
		},
	)
	ep := &streams.Episode{RatingKey: "100", GrandparentRatingKey: "42"}
	sched.processRecentlyAddedEpisode(t.Context(), ep)
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
		Deps{
			Plex:       plx,
			Cache:      c,
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       syncer,
			SaveCache:  nil,
		},
	)
	ep := &streams.Episode{RatingKey: "100", GrandparentRatingKey: "42"}
	sched.processRecentlyAddedEpisode(t.Context(), ep)
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
		Deps{
			Plex:       plx,
			Cache:      c,
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       syncer,
			SaveCache:  nil,
		},
	)
	sched.processRecentlyAdded(t.Context(), time.Now().Unix())
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
		Config{Enable: true, Ignore: ignore.New(ignore.Config{Libraries: []string{"Kids"}})},
		Deps{
			Plex:       plx,
			Cache:      c,
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       syncer,
			SaveCache:  nil,
		},
	)
	sched.processRecentlyAdded(t.Context(), time.Now().Unix())
	if syncer.processCalls.Load() != 1 {
		t.Errorf("ProcessNewOrUpdated called %d times; want 1 (Kids section ignored)", syncer.processCalls.Load())
	}
}

// ---------------------------------------------------------------------------
// deepAnalysisCore — persistence (last-run stamp + cache flush)
// ---------------------------------------------------------------------------

// TestDeepAnalysisCore_SetsLastRunAndFlushesCache pins the deferred persistence in
// deepAnalysisCore: it records the last-run stamp (the documented cold-restart
// idempotency guard) and flushes the cache exactly once via saveCache. A nil
// saveCache must be a no-op, not a panic.
func TestDeepAnalysisCore_SetsLastRunAndFlushesCache(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{}
	st := testStamp(t)
	if _, known := st.Last(); known {
		t.Fatal("precondition: fresh stamp must report no recorded run")
	}
	var saveCalls atomic.Int64
	sched := New(
		Config{Enable: true},
		Deps{
			Plex:       plx,
			Cache:      fakeapi.NewCache(),
			Stamp:      st,
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       &fakeSyncer{},
			SaveCache:  func() error { saveCalls.Add(1); return nil },
		},
	)

	sched.deepAnalysisCore(t.Context())

	if _, known := st.Last(); !known {
		t.Error("deepAnalysisCore did not record the last-run stamp")
	}
	if got := saveCalls.Load(); got != 1 {
		t.Errorf("saveCache called %d times; want 1", got)
	}

	// A nil saveCache must not panic.
	schedNilSave := New(
		Config{Enable: true},
		Deps{
			Plex:       plx,
			Cache:      fakeapi.NewCache(),
			Stamp:      testStamp(t),
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       &fakeSyncer{},
			SaveCache:  nil,
		},
	)
	schedNilSave.deepAnalysisCore(t.Context())
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
		Deps{
			Plex:       plx,
			Cache:      fakeapi.NewCache(),
			Stamp:      testStamp(t),
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       &fakeSyncer{},
			SaveCache:  nil,
		},
	)
	sched.Run(t.Context())
	if plx.Calls.Load() != 0 {
		t.Errorf("Run(disabled) made %d Plex calls; want 0", plx.Calls.Load())
	}
}

// TestRun_NonPositiveIntervalIsDisabled pins the interval half of the enable
// gate. The Enable=false test above short-circuits before the interval is ever
// read, so nothing else covers a configured-but-unusable interval: without this
// gate a zero period reaches the scheduling loop, where the meaning of "every
// 0" is whatever the loop happens to do with it.
func TestRun_NonPositiveIntervalIsDisabled(t *testing.T) {
	t.Parallel()
	tests := []struct {
		name     string
		interval time.Duration
	}{
		{"zero", 0},
		{"negative", -time.Hour},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			t.Parallel()
			plx := &fakeapi.Plex{}
			sched := New(
				Config{Enable: true, Interval: tt.interval},
				Deps{
					Plex:       plx,
					Cache:      fakeapi.NewCache(),
					Stamp:      testStamp(t),
					UserClient: func(_ string) EpisodeReader { return plx },
					Sync:       &fakeSyncer{},
					SaveCache:  nil,
				},
			)
			sched.Run(t.Context())
			if got := plx.Calls.Load(); got != 0 {
				t.Errorf("Run(Enable=true, Interval=%v) made %d Plex calls; want 0 (a non-positive interval disables the scheduler)",
					tt.interval, got)
			}
		})
	}
}

// TestRun_RunsInitialAnalysisWhenNeverRun verifies the "run immediately when the
// last-run record is absent" branch. A pre-cancelled context makes the scheduling
// loop return as soon as the initial pass is dispatched, so the timer wait
// (timing-bound, intentionally untested) is never entered. Because the initial
// pass therefore runs under an already-cancelled ctx, the h-f1 watermark-on-cancel
// guard means it does NOT record the run; the proof that the initial
// branch fired is the Plex History + ShowSections queries below (fetched at the
// top of the pass before any ctx short-circuit). Recording on a COMPLETED
// pass is pinned separately by
// TestDeepAnalysisCore_CancelledPassLeavesWatermarkUnchanged.
func TestRun_RunsInitialAnalysisWhenNeverRun(t *testing.T) {
	t.Parallel()
	plx := &fakeapi.Plex{}
	sched := New(
		Config{Enable: true, Interval: 24 * time.Hour},
		Deps{
			Plex:       plx,
			Cache:      fakeapi.NewCache(),
			Stamp:      testStamp(t), // no record -> initial pass should fire
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       &fakeSyncer{},
			SaveCache:  nil,
		},
	)
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // loop returns on ctx.Done() right after the initial pass
	sched.Run(ctx)

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

// TestRun_InitialPassDecisionFromStamp pins Run's initial-pass gate
// (stamp.Due(Interval, now, CountFailed)). The other Run tests cover only the
// disabled short-circuit and the no-record arm; neither stages an existing
// record, so the documented cold-restart idempotency guard (a record newer
// than one interval must NOT double-run the analysis on restart) and its
// stale-record catch-up companion have no coverage -- and are not covered
// transitively, since the deepAnalysisCore-level tests call deepAnalysisCore
// directly and bypass this gate. A pre-cancelled context makes Run return on
// ctx.Done() right after the gate, so the timing-bound ticker loop is never
// entered (the technique TestRun_RunsInitialAnalysisWhenNeverRun uses).
func TestRun_InitialPassDecisionFromStamp(t *testing.T) {
	t.Parallel()
	t.Run("recent record skips the initial pass", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{}
		st := testStamp(t)
		if err := st.Record(true); err != nil { // ran within the last interval
			t.Fatalf("seed stamp: %v", err)
		}
		sched := New(
			Config{Enable: true, Interval: 24 * time.Hour},
			Deps{
				Plex:       plx,
				Cache:      fakeapi.NewCache(),
				Stamp:      st,
				UserClient: func(_ string) EpisodeReader { return plx },
				Sync:       &fakeSyncer{},
				SaveCache:  nil,
			},
		)
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // pre-cancelled on purpose; Background, not t.Context()
		sched.Run(ctx)
		if got := plx.Calls.Load(); got != 0 {
			t.Errorf("Run made %d Plex calls with a recent last-run record; want 0 (the cold-restart guard must skip the initial pass, not re-run a full sweep)", got)
		}
	})
	t.Run("stale record runs a catch-up pass", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{}
		st := stampAt(t, time.Now().Add(-72*time.Hour)) // older than the 24h interval
		sched := New(
			Config{Enable: true, Interval: 24 * time.Hour},
			Deps{
				Plex:       plx,
				Cache:      fakeapi.NewCache(),
				Stamp:      st,
				UserClient: func(_ string) EpisodeReader { return plx },
				Sync:       &fakeSyncer{},
				SaveCache:  nil,
			},
		)
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // pre-cancelled on purpose; Background, not t.Context()
		sched.Run(ctx)
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
			t.Errorf("stale record did not trigger a catch-up pass (calls=%v); want History and ShowSections", names)
		}
	})
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
		Deps{
			Plex:       plx,
			Cache:      fakeapi.NewCache(),
			UserClient: func(_ string) EpisodeReader { return plx.Plex },
			Sync:       syncer,
			SaveCache:  nil,
		},
	)
	out := captureSlog(t, func() {
		sched.processRecentHistory(t.Context(), time.Now().Unix())
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
		Deps{
			Plex:       plx,
			Cache:      fakeapi.NewCache(),
			UserClient: func(_ string) EpisodeReader { return plx.Plex },
			Sync:       syncer,
			SaveCache:  nil,
		},
	)
	out := captureSlog(t, func() {
		sched.processRecentlyAdded(t.Context(), time.Now().Unix())
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
		Deps{
			Plex:       plx,
			Cache:      fakeapi.NewCache(),
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       &fakeSyncer{},
			SaveCache:  nil,
		},
	)

	out := captureSlog(t, func() {
		sched.processRecentHistory(t.Context(), time.Now().Unix())
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
		Deps{
			Plex:       plx,
			Cache:      c,
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       syncer,
			SaveCache:  nil,
		},
	)
	ep := &streams.Episode{RatingKey: "100"}
	out := captureSlog(t, func() {
		sched.processRecentlyAddedEpisode(t.Context(), ep)
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
		Deps{
			Plex:       plx,
			Cache:      c,
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       syncer,
			SaveCache:  nil,
		},
	)
	ep := &streams.Episode{RatingKey: "404"}
	out := captureSlog(t, func() {
		sched.processRecentlyAddedEpisode(t.Context(), ep)
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
		Deps{
			Plex:       plx,
			Cache:      fakeapi.NewCache(),
			Stamp:      testStamp(t),
			UserClient: func(_ string) EpisodeReader { return plx.Plex },
			Sync:       &fakeSyncer{},
			SaveCache:  nil,
		},
	)

	out := captureSlog(t, func() {
		var wg sync.WaitGroup
		wg.Go(func() {
			sched.deepAnalysis(t.Context()) // winner: blocks inside History
		})

		// Wait until the winner is inside the singleflight callback (blocked
		// in History), so it owns the "deep_analysis" key before the loser
		// calls Do.
		<-plx.entered

		loserJoined := make(chan struct{})
		wg.Go(func() {
			close(loserJoined)              // about to enter Do; hand the scheduler over below
			sched.deepAnalysis(t.Context()) // loser: collapses, logs skip
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
		Deps{
			Plex:       plx,
			Cache:      fakeapi.NewCache(),
			UserClient: func(_ string) EpisodeReader { return plx.Plex },
			Sync:       syncer,
			SaveCache:  nil,
		},
	)
	out := captureSlog(t, func() {
		sched.processRecentlyAdded(t.Context(), time.Now().Unix())
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
// (no admin-client fallback, no Episode fetch, no ReconcileWithIntent) and
// the skip is logged. Not parallel: captureSlog mutates the global logger.
func TestProcessHistoryItem_NilPerUserClientSkips(t *testing.T) {
	plx := &fakeapi.Plex{
		HistoryItems: []plex.HistoryItem{{RatingKey: "300", Type: "episode"}},
	}
	syncer := &fakeSyncer{}
	sched := New(
		Config{Enable: true},
		Deps{
			Plex:       plx,
			Cache:      fakeapi.NewCache(),
			UserClient: func(_ string) EpisodeReader { return nil },
			Sync:       syncer,
			SaveCache:  nil,
		},
	)
	out := captureSlog(t, func() {
		sched.processRecentHistory(t.Context(), time.Now().Unix())
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
// branch: a failing saveCache still records the last-run stamp and logs the
// "cache save failed" WARN rather than swallowing the error. Companion to
// TestDeepAnalysisCore_SetsLastRunAndFlushesCache. Not parallel: captureSlog
// mutates the global logger.
func TestDeepAnalysisCore_SaveCacheErrorWarns(t *testing.T) {
	plx := &fakeapi.Plex{}
	st := testStamp(t)
	sched := New(
		Config{Enable: true},
		Deps{
			Plex:       plx,
			Cache:      fakeapi.NewCache(),
			Stamp:      st,
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       &fakeSyncer{},
			SaveCache:  func() error { return errors.New("disk full") },
		},
	)
	out := captureSlog(t, func() {
		sched.deepAnalysisCore(t.Context())
	})
	if !strings.Contains(out, "cache save failed") {
		t.Errorf("missing cache-save-failure WARN when saveCache errors; log: %q", out)
	}
	if _, known := st.Last(); !known {
		t.Error("last-run stamp must still be recorded even when the cache flush fails")
	}
}

// TestDeepAnalysisCore_StampRecordFailureWarnsOnly pins the deferred stamp
// write's error branch: a Record that cannot write (its directory is gone)
// logs a WARN and never fails the pass — the completion line still logs and
// the cache flush still runs. Not parallel: captureSlog mutates the global
// logger.
func TestDeepAnalysisCore_StampRecordFailureWarnsOnly(t *testing.T) {
	plx := &fakeapi.Plex{}
	st := scheduler.NewStamp(filepath.Join(t.TempDir(), "missing-dir", "last-run"))
	var saveCalls atomic.Int64
	sched := New(
		Config{Enable: true},
		Deps{
			Plex:       plx,
			Cache:      fakeapi.NewCache(),
			Stamp:      st,
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       &fakeSyncer{},
			SaveCache:  func() error { saveCalls.Add(1); return nil },
		},
	)
	out := captureSlog(t, func() {
		sched.deepAnalysisCore(t.Context())
	})
	if !strings.Contains(out, "last-run stamp write failed") {
		t.Errorf("missing stamp-write-failure WARN; log: %q", out)
	}
	if !strings.Contains(out, "deep analysis completed") {
		t.Errorf("pass did not run to completion despite the stamp write failing; log: %q", out)
	}
	if got := saveCalls.Load(); got != 1 {
		t.Errorf("saveCache called %d times after a failed stamp write; want 1 (the flush must still run)", got)
	}
}

// TestScheduler_CancelledContextSkipsPerItemWork pins graceful-shutdown
// responsiveness: a cancelled context short-circuits the deep-analysis feeders
// so no per-item work runs (regression guard for the ctx checks in feedHistory
// and feedRecentlyAdded).
func TestScheduler_CancelledContextSkipsPerItemWork(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	cancel() // pre-cancelled on purpose; also Background because the parallel subtests below outlive this parent, and t.Context() would already be cancelled for a different reason
	t.Run("history feeder", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{
			HistoryItems: []plex.HistoryItem{{RatingKey: "1", Type: "episode"}, {RatingKey: "2", Type: "episode"}},
			EpisodeByKey: map[string]*streams.Episode{"1": {RatingKey: "1"}, "2": {RatingKey: "2"}},
		}
		syncer := &fakeSyncer{}
		sched := New(Config{Enable: true}, Deps{
			Plex:       plx,
			Cache:      fakeapi.NewCache(),
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       syncer,
			SaveCache:  nil,
		})
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
		sched := New(Config{Enable: true}, Deps{
			Plex:       plx,
			Cache:      fakeapi.NewCache(),
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       syncer,
			SaveCache:  nil,
		})
		sched.processRecentlyAdded(ctx, time.Now().Unix())
		if syncer.processCalls.Load() != 0 {
			t.Errorf("ProcessNewOrUpdated called %d times under a cancelled context; want 0", syncer.processCalls.Load())
		}
	})
}

// TestFeedHistory_PreFiltersNonEpisodeAndIgnoredLibrary pins feedHistory's two
// pre-dispatch filters: a non-episode history item (a movie) and an episode
// from an ignored library must both be dropped before any worker fetches them,
// so only the eligible TV episode is processed. The symmetric recently-added
// path has TestProcessRecentlyAdded_HonorsIgnoreLibraries; the history path's
// identical filters were unpinned. changeCalls catches an ignore-library
// regression (the Kids episode would then be synced); Calls catches a
// non-episode regression (the movie would then trigger an Episode fetch).
func TestFeedHistory_PreFiltersNonEpisodeAndIgnoredLibrary(t *testing.T) {
	t.Parallel()
	items := []plex.HistoryItem{
		{RatingKey: "1", Type: "movie", LibraryTitle: "Movies"},
		{RatingKey: "2", Type: "episode", LibraryTitle: "Kids"},
		{RatingKey: "3", Type: "episode", LibraryTitle: "TV"},
	}
	plx := &fakeapi.Plex{
		HistoryItems: items,
		EpisodeByKey: map[string]*streams.Episode{
			"2": {RatingKey: "2"},
			"3": {RatingKey: "3"},
		},
	}
	syncer := &fakeSyncer{}
	sched := New(
		Config{Enable: true, Ignore: ignore.New(ignore.Config{Libraries: []string{"Kids"}})},
		Deps{
			Plex:       plx,
			Cache:      fakeapi.NewCache(),
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       syncer,
			SaveCache:  nil,
		},
	)

	sched.processRecentHistory(t.Context(), time.Now().Unix())

	if got := syncer.changeCalls.Load(); got != 1 {
		t.Errorf("ChangeTracks called %d times; want 1 (movie type and ignored-library episode must be filtered before dispatch)", got)
	}
	if got := plx.Calls.Load(); got != 2 {
		t.Errorf("Plex called %d times; want 2 (1 History + 1 Episode for the TV episode only; filtered items must never trigger a fetch)", got)
	}
}

// TestScheduler_ContextCanceledFetchIsDebugNotWarn pins the shutdown log
// contract: a History or ShowSections fetch failing with context.Canceled
// (graceful shutdown) must emit the Debug "cancelled during shutdown" line, NOT
// the "failed to fetch" WARN. The WARN keys are a Loki-alert contract, so a
// spurious one on every shutdown would pollute it. Reuses the fetchErrPlex
// wrapper and captureSlog helper already in this file. Not parallel: captureSlog
// mutates the process-global default logger.
func TestScheduler_ContextCanceledFetchIsDebugNotWarn(t *testing.T) {
	t.Run("history", func(t *testing.T) {
		plx := &fetchErrPlex{Plex: &fakeapi.Plex{}, historyErr: context.Canceled}
		sched := New(
			Config{Enable: true},
			Deps{
				Plex:       plx,
				Cache:      fakeapi.NewCache(),
				UserClient: func(_ string) EpisodeReader { return plx.Plex },
				Sync:       &fakeSyncer{},
				SaveCache:  nil,
			},
		)
		out := captureSlog(t, func() {
			sched.processRecentHistory(t.Context(), time.Now().Unix())
		})
		if strings.Contains(out, "scheduler: failed to fetch history") {
			t.Errorf("context.Canceled must not emit the fetch-failure WARN; log: %q", out)
		}
		if !strings.Contains(out, "history fetch cancelled during shutdown") {
			t.Errorf("context.Canceled during shutdown should emit the Debug line; log: %q", out)
		}
	})
	t.Run("sections", func(t *testing.T) {
		plx := &fetchErrPlex{Plex: &fakeapi.Plex{}, sectionsErr: context.Canceled}
		sched := New(
			Config{Enable: true},
			Deps{
				Plex:       plx,
				Cache:      fakeapi.NewCache(),
				UserClient: func(_ string) EpisodeReader { return plx.Plex },
				Sync:       &fakeSyncer{},
				SaveCache:  nil,
			},
		)
		out := captureSlog(t, func() {
			sched.processRecentlyAdded(t.Context(), time.Now().Unix())
		})
		if strings.Contains(out, "scheduler: failed to fetch sections") {
			t.Errorf("context.Canceled must not emit the fetch-failure WARN; log: %q", out)
		}
		if !strings.Contains(out, "sections fetch cancelled during shutdown") {
			t.Errorf("context.Canceled during shutdown should emit the Debug line; log: %q", out)
		}
	})
}

// TestProcessRecentHistory_PartialItemFailureWarnsWithCounts pins the aggregate
// replay-incomplete WARN on the history path: when some history items fail their
// per-user Episode fetch (below the circuit-breaker threshold, so the pass runs
// to completion), processRecentHistory emits one WARN reporting failed/total item
// counts. This mirrors the recently-added path's
// TestFeedRecentlyAdded_PartialSectionFailureWarnsWithCounts; the history-path
// WARN has no assertion pinning its Loki-alert key + attributes. A single worker
// makes failed_items deterministic and keeps the consecutive-error total (3)
// below maxConsecutiveErrors (5), so the breaker never trips and all items are
// processed. Not parallel: captureSlog mutates the process-global default logger.
func TestProcessRecentHistory_PartialItemFailureWarnsWithCounts(t *testing.T) {
	items := []plex.HistoryItem{
		{RatingKey: "1", Type: "episode"},
		{RatingKey: "2", Type: "episode"},
		{RatingKey: "3", Type: "episode"},
	}
	plx := &fakeapi.Plex{HistoryItems: items, EpisodeErr: errors.New("fetch boom")}
	sched := New(
		Config{Enable: true},
		Deps{
			Plex:       plx,
			Cache:      fakeapi.NewCache(),
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       &fakeSyncer{},
			SaveCache:  nil,
		},
	)
	sched.workers = 1 // serial -> deterministic failed_items; 3 failures < breaker threshold (5)
	out := captureSlog(t, func() {
		sched.processRecentHistory(t.Context(), time.Now().Unix())
	})
	if !strings.Contains(out, "scheduler: recent-history replay incomplete, some items failed to fetch") {
		t.Errorf("missing aggregate replay-incomplete WARN on partial item failure; log: %q", out)
	}
	if !strings.Contains(out, "failed_items=3") {
		t.Errorf("aggregate WARN must report failed_items=3; log: %q", out)
	}
	if !strings.Contains(out, "total_items=3") {
		t.Errorf("aggregate WARN must report total_items=3; log: %q", out)
	}
}

// ---------------------------------------------------------------------------
// deepAnalysisCore — extended look-back window (l-f11 resolution)
// ---------------------------------------------------------------------------

// sinceCapturePlex wraps fakeapi.Plex and records the sinceUnix argument
// passed to History -- the seam that lets a test assert the deep-analysis
// look-back window start (mirrors the fetchErrPlex / recentlyAddedErrPlex /
// blockingDeepAnalysisPlex wrappers already in this file). Only History is
// overridden; every other method is promoted from the embedded *fakeapi.Plex.
type sinceCapturePlex struct {
	*fakeapi.Plex
	historySince atomic.Int64
}

func (p *sinceCapturePlex) History(ctx context.Context, since int64) ([]plex.HistoryItem, error) {
	p.historySince.Store(since)
	return p.Plex.History(ctx, since)
}

// TestDeepAnalysisCore_ExtendsLookbackBeyond24hFromLastRun pins the extended
// look-back window: when the previous run is older than 24h (DEEP_SCAN_INTERVAL
// > 24h, or a restart after a long downtime), the replay look-back extends to
// the full since-last-run gap instead of the fixed 24h floor, so the span
// between 24h and the interval is not silently skipped by the safety net.
// deepAnalysisCore reads stamp.Last() (the PREVIOUS run -- this run is only
// recorded in the deferred Record after the body), extends lookback to
// max(24h, time.Since(last)), and feeds the resulting sinceUnix to History.
// The two older deepAnalysisCore tests use a fresh (no record) stamp, so they
// exercise only the 24h floor; the extend branch and the resulting window
// value were unasserted.
func TestDeepAnalysisCore_ExtendsLookbackBeyond24hFromLastRun(t *testing.T) {
	t.Parallel()
	plx := &sinceCapturePlex{Plex: &fakeapi.Plex{}}
	sched := New(
		Config{Enable: true},
		Deps{
			Plex:       plx,
			Cache:      fakeapi.NewCache(),
			Stamp:      stampAt(t, time.Now().Add(-72*time.Hour)), // previous run 72h ago
			UserClient: func(_ string) EpisodeReader { return plx.Plex },
			Sync:       &fakeSyncer{},
			SaveCache:  nil,
		},
	)

	sched.deepAnalysisCore(t.Context())
	after := time.Now()

	windowStart := time.Unix(plx.historySince.Load(), 0)
	lookback := after.Sub(windowStart)
	// ~72h expected. The lower bound well above the 24h floor proves the branch
	// extended the window; the upper bound proves it stayed bounded by the
	// since-last-run gap rather than an unbounded/incorrect value.
	if lookback < 48*time.Hour {
		t.Errorf("look-back %v was not extended past the 24h floor; the >24h gap since the last run is unswept (window start %v)",
			lookback, windowStart.UTC())
	}
	if lookback > 96*time.Hour {
		t.Errorf("look-back %v exceeds the ~72h since-last-run gap (window start %v)",
			lookback, windowStart.UTC())
	}
}

// TestDeepAnalysisCore_LookbackFloorIsOneDayOnFirstRun pins the 24h floor at
// the other end of the same window calculation: with no previous run recorded —
// first boot, or a deleted record file — the replay still covers a full
// recent day. This is the arm the two older deepAnalysisCore tests exercise but
// never assert: a floor that collapsed would leave the safety-net pass fetching
// an empty window and repairing nothing, silently, forever.
func TestDeepAnalysisCore_LookbackFloorIsOneDayOnFirstRun(t *testing.T) {
	t.Parallel()
	plx := &sinceCapturePlex{Plex: &fakeapi.Plex{}}
	sched := New(
		Config{Enable: true},
		Deps{
			Plex:       plx,
			Cache:      fakeapi.NewCache(),
			Stamp:      testStamp(t), // no record -> the floor decides the window
			UserClient: func(_ string) EpisodeReader { return plx.Plex },
			Sync:       &fakeSyncer{},
			SaveCache:  nil,
		},
	)

	sched.deepAnalysisCore(t.Context())
	after := time.Now()

	windowStart := time.Unix(plx.historySince.Load(), 0)
	lookback := after.Sub(windowStart)
	if lookback < 23*time.Hour {
		t.Errorf("first-run look-back = %v, want ~24h; a window this short leaves recent plays unswept (window start %v)",
			lookback, windowStart.UTC())
	}
	if lookback > 25*time.Hour {
		t.Errorf("first-run look-back = %v, want ~24h; the floor must not widen the window on its own (window start %v)",
			lookback, windowStart.UTC())
	}
}

// ---------------------------------------------------------------------------
// deepAnalysisCore — watermark-on-cancel guard (h-f1 resolution)
// ---------------------------------------------------------------------------

// TestDeepAnalysisCore_CancelledPassLeavesWatermarkUnchanged pins the h-f1
// watermark-on-cancel guard: deepAnalysisCore's deferred stamp.Record runs
// ONLY when the pass completed (ctx.Err()==nil). A pass cancelled mid-flight
// (graceful shutdown) fetches history and recently-added newest-first and may
// leave the OLDER end of its window unprocessed, so recording it would make
// the next run's dynamic look-back (l-f11) start past those unprocessed
// events -- permanently skipping them. The record must therefore stay at the
// previous completed run's value on a cancelled pass and advance on a
// completed one. The saveCache flush stays UNGUARDED (persisting partial
// learning is harmless; main.go re-saves on shutdown), which the cancelled
// subtest's saveCalls check keeps pinned so a regression that wrongly guards
// the whole defer body is caught too.
func TestDeepAnalysisCore_CancelledPassLeavesWatermarkUnchanged(t *testing.T) {
	t.Parallel()
	t.Run("cancelled pass does not advance the record", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{}
		prev := time.Now().Add(-72 * time.Hour) // previous COMPLETED run's record
		st := stampAt(t, prev)
		var saveCalls atomic.Int64
		sched := New(
			Config{Enable: true},
			Deps{
				Plex:       plx,
				Cache:      fakeapi.NewCache(),
				Stamp:      st,
				UserClient: func(_ string) EpisodeReader { return plx },
				Sync:       &fakeSyncer{},
				SaveCache:  func() error { saveCalls.Add(1); return nil },
			},
		)
		ctx, cancel := context.WithCancel(context.Background())
		cancel() // graceful-shutdown: the pass sees a cancelled context

		sched.deepAnalysisCore(ctx)

		if got := stampTime(t, st); !got.Equal(prev) {
			t.Errorf("cancelled pass advanced the record to %v; want it unchanged at the previous completed run %v (advancing would skip the unprocessed older window)",
				got.UTC(), prev.UTC())
		}
		// saveCache stays UNGUARDED: a cancelled pass still flushes partial
		// learning. A regression that wrongly guards the whole defer body
		// would drop this flush.
		if got := saveCalls.Load(); got != 1 {
			t.Errorf("saveCache called %d times on a cancelled pass; want 1 (the flush is intentionally unguarded)", got)
		}
	})
	t.Run("completed pass advances the record", func(t *testing.T) {
		t.Parallel()
		plx := &fakeapi.Plex{}
		prev := time.Now().Add(-72 * time.Hour)
		st := stampAt(t, prev)
		sched := New(
			Config{Enable: true},
			Deps{
				Plex:       plx,
				Cache:      fakeapi.NewCache(),
				Stamp:      st,
				UserClient: func(_ string) EpisodeReader { return plx },
				Sync:       &fakeSyncer{},
				SaveCache:  nil,
			},
		)
		before := time.Now()

		sched.deepAnalysisCore(t.Context())

		got := stampTime(t, st)
		if !got.After(prev) {
			t.Errorf("completed pass did not advance the record: got %v, still <= previous run %v", got.UTC(), prev.UTC())
		}
		if got.Before(before) {
			t.Errorf("completed pass set the record to %v, before the run started %v", got.UTC(), before.UTC())
		}
	})
}

// TestDeepAnalysisCore_IncompletePassLeavesWatermarkUnchanged pins the uniform
// completeness gate that generalises the h-f1 ctx-cancel guard: deepAnalysisCore
// records the run ONLY when the pass fully covered its look-back window.
// Because History/RecentlyAdded are swept newest-first, ANY early exit leaves
// the OLDER end of the window unprocessed, so recording it would make the next
// run's dynamic look-back start past those unswept events and drop them
// permanently. The three incomplete-pass triggers below (in addition to the
// ctx-cancel case, which has its own test) must all leave the record unchanged:
// a history circuit-breaker abort, a History fetch/overflow error, and a
// recently-added per-section fetch failure.
func TestDeepAnalysisCore_IncompletePassLeavesWatermarkUnchanged(t *testing.T) {
	t.Parallel()

	// prev is the previous COMPLETED run's record; every subtest asserts it is
	// left untouched by an incomplete pass.
	newSched := func(plx plexReader, reader func(string) EpisodeReader, st *scheduler.Stamp) *Scheduler {
		return New(Config{Enable: true}, Deps{Plex: plx, Cache: fakeapi.NewCache(), Stamp: st, UserClient: reader, Sync: &fakeSyncer{}})
	}

	t.Run("history circuit-breaker abort", func(t *testing.T) {
		t.Parallel()
		// 20 episode history items whose per-user Episode fetch always fails →
		// the consecutive-error breaker trips and feedHistory aborts before
		// feeding the whole window (older items never processed).
		items := make([]plex.HistoryItem, 20)
		for i := range items {
			items[i] = plex.HistoryItem{AccountID: 1, RatingKey: strconv.Itoa(1000 + i), Type: plex.TypeEpisode}
		}
		plx := &fakeapi.Plex{HistoryItems: items, EpisodeErr: errors.New("fetch boom")}
		prev := time.Now().Add(-72 * time.Hour)
		st := stampAt(t, prev)
		sched := newSched(plx, func(_ string) EpisodeReader { return plx }, st)

		sched.deepAnalysisCore(t.Context())

		if got := stampTime(t, st); !got.Equal(prev) {
			t.Errorf("breaker-abort pass advanced the record to %v; want unchanged at %v (older window unswept)", got.UTC(), prev.UTC())
		}
	})

	t.Run("history fetch/overflow error", func(t *testing.T) {
		t.Parallel()
		// A History error models the 10MB-overflow case (errBodyOverCap): zero
		// items replayed, so the record must not advance.
		plx := &fetchErrPlex{Plex: &fakeapi.Plex{}, historyErr: errors.New("body over cap")}
		prev := time.Now().Add(-72 * time.Hour)
		st := stampAt(t, prev)
		sched := newSched(plx, func(_ string) EpisodeReader { return plx.Plex }, st)

		sched.deepAnalysisCore(t.Context())

		if got := stampTime(t, st); !got.Equal(prev) {
			t.Errorf("history-error pass advanced the record to %v; want unchanged at %v", got.UTC(), prev.UTC())
		}
	})

	t.Run("recently-added section fetch failure", func(t *testing.T) {
		t.Parallel()
		// History is empty (complete), but one recently-added section fails its
		// fetch, leaving that section's window unswept → pass incomplete.
		base := &fakeapi.Plex{Sections: []plex.Section{{Key: "1", Title: "TV"}}}
		plx := &recentlyAddedErrPlex{Plex: base, failSections: map[string]bool{"1": true}}
		prev := time.Now().Add(-72 * time.Hour)
		st := stampAt(t, prev)
		sched := newSched(plx, func(_ string) EpisodeReader { return plx.Plex }, st)

		sched.deepAnalysisCore(t.Context())

		if got := stampTime(t, st); !got.Equal(prev) {
			t.Errorf("section-failure pass advanced the record to %v; want unchanged at %v", got.UTC(), prev.UTC())
		}
	})
}

// TestDeepAnalysisCore_CapsLookback pins the maxDeepAnalysisLookback cap: a
// record far older than the cap (a long outage or a very large
// DEEP_SCAN_INTERVAL) must not grow the non-paginated History/RecentlyAdded
// window without bound. The look-back is clamped to ~30 days regardless of how
// old the previous run is.
func TestDeepAnalysisCore_CapsLookback(t *testing.T) {
	t.Parallel()
	plx := &sinceCapturePlex{Plex: &fakeapi.Plex{}}
	sched := New(
		Config{Enable: true},
		Deps{
			Plex:       plx,
			Cache:      fakeapi.NewCache(),
			Stamp:      stampAt(t, time.Now().Add(-60*24*time.Hour)), // 60 days ago, well past the 30d cap
			UserClient: func(_ string) EpisodeReader { return plx.Plex },
			Sync:       &fakeSyncer{},
			SaveCache:  nil,
		},
	)

	sched.deepAnalysisCore(t.Context())
	after := time.Now()

	windowStart := time.Unix(plx.historySince.Load(), 0)
	lookback := after.Sub(windowStart)
	if lookback > 31*24*time.Hour {
		t.Errorf("look-back %v exceeds the 30d cap; a 60d-old record must be clamped (window start %v)", lookback, windowStart.UTC())
	}
	if lookback < 29*24*time.Hour {
		t.Errorf("look-back %v is below the 30d cap; expected the window clamped to ~30d (window start %v)", lookback, windowStart.UTC())
	}
}

// TestDeepAnalysisCore_ScatteredHistoryFailuresBelowBreakerStillAdvanceMarker
// pins the completeness gate's accepted-loss boundary: scattered per-user
// Episode() fetch failures that stay BELOW the circuit-breaker threshold do NOT
// block completion, so the pass is complete and the run is recorded.
// processRecentHistory keys completeness on fedAll (every item fed, breaker did
// not abort), NOT on totalErrors==0 -- its comment documents this as "the
// design's accepted skip-and-continue loss". The breaker-ABORT side is pinned by
// TestDeepAnalysisCore_IncompletePassLeavesWatermarkUnchanged; this pins the
// complement. Without it a regression coupling completeness to the error count
// (return fedAll && totalErrors.Load()==0 && ctx.Err()==nil) survives every test
// -- statement coverage of processRecentHistory/deepAnalysisCore is already
// 100% -- yet makes any pass with a single transient item failure never record,
// growing the look-back to the 30d cap and re-sweeping it every tick.
func TestDeepAnalysisCore_ScatteredHistoryFailuresBelowBreakerStillAdvanceMarker(t *testing.T) {
	t.Parallel()
	// 3 episode items whose per-user Episode fetch always fails: 3 consecutive
	// errors stay below maxConsecutiveErrors (5), so the breaker never trips,
	// feedHistory feeds every item (fedAll=true), and the pass is complete
	// despite the 3 accepted-loss failures. workers=1 keeps the count
	// deterministic. Sections are empty, so the recently-added leg is trivially
	// complete and the record's advance is governed solely by the history leg.
	items := []plex.HistoryItem{
		{AccountID: 1, RatingKey: "1", Type: plex.TypeEpisode},
		{AccountID: 1, RatingKey: "2", Type: plex.TypeEpisode},
		{AccountID: 1, RatingKey: "3", Type: plex.TypeEpisode},
	}
	plx := &fakeapi.Plex{HistoryItems: items, EpisodeErr: errors.New("fetch boom")}
	prev := time.Now().Add(-72 * time.Hour) // previous COMPLETED run's record
	st := stampAt(t, prev)
	sched := New(
		Config{Enable: true},
		Deps{
			Plex:       plx,
			Cache:      fakeapi.NewCache(),
			Stamp:      st,
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       &fakeSyncer{},
			SaveCache:  nil,
		},
	)
	sched.workers = 1 // serial -> 3 failures deterministic, below the breaker threshold

	sched.deepAnalysisCore(t.Context())

	if got := stampTime(t, st); !got.After(prev) {
		t.Errorf("pass with scattered below-breaker item failures did not advance the record: got %v, still <= previous run %v (accepted-loss failures must not block completion)", got.UTC(), prev.UTC())
	}
}

// TestDeepAnalysis_CleanPassRaisesNoIncompleteWarning pins the negative half of
// both partial-failure summaries: a pass where every fetch succeeded raises
// neither. Both lines are WARNs an operator (and a Loki alert) reads as "this
// sweep lost items", so one that also fired on a healthy pass would make the
// signal worthless — and the healthy pass is the one that runs every day.
// Not parallel: captureSlog mutates the process-global default logger.
func TestDeepAnalysis_CleanPassRaisesNoIncompleteWarning(t *testing.T) {
	plx := &fakeapi.Plex{
		HistoryItems: []plex.HistoryItem{
			{AccountID: 1, RatingKey: "100", Type: plex.TypeEpisode},
		},
		Sections: []plex.Section{{Key: "1", Title: "TV"}},
		RecentlyAddedBySec: map[string][]streams.Episode{
			"1": {{RatingKey: "200"}},
		},
		EpisodeByKey: map[string]*streams.Episode{
			"100": {RatingKey: "100"},
			"200": {RatingKey: "200"},
		},
	}
	sched := New(
		Config{Enable: true},
		Deps{
			Plex:       plx,
			Cache:      fakeapi.NewCache(),
			Stamp:      testStamp(t),
			UserClient: func(_ string) EpisodeReader { return plx },
			Sync:       &fakeSyncer{},
			SaveCache:  nil,
		},
	)

	out := captureSlog(t, func() {
		sched.deepAnalysisCore(t.Context())
	})

	if strings.Contains(out, "recent-history replay incomplete") {
		t.Errorf("a pass with no failed history item raised the replay-incomplete WARN; log: %q", out)
	}
	if strings.Contains(out, "recently-added sweep incomplete") {
		t.Errorf("a pass with no failed section raised the sweep-incomplete WARN; log: %q", out)
	}
}
