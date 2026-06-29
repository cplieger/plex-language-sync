package fakeapi

import (
	"sync"
	"testing"
	"time"

	"github.com/cplieger/plex-language-sync/internal/api"
)

// RunCacheContract exercises the api.Cache contract against any
// implementation. Both cache.Cache and fakeapi.Cache must pass.
func RunCacheContract(t *testing.T, c api.Cache) {
	t.Helper()

	t.Run("SetGet_roundtrip", func(t *testing.T) {
		c.LearnLanguageProfile("u1", "eng", "fra")
		got, ok := c.SubtitleLangForAudio("u1", "eng")
		if !ok || got != "fra" {
			t.Errorf("SubtitleLangForAudio = (%q, %v), want (fra, true)", got, ok)
		}
	})

	t.Run("processed_roundtrip", func(t *testing.T) {
		if c.WasRecentlyProcessed("contract-key") {
			t.Error("fresh key should not be recently processed")
		}
		c.MarkProcessed("contract-key")
		if !c.WasRecentlyProcessed("contract-key") {
			t.Error("after MarkProcessed, WasRecentlyProcessed should be true")
		}
	})

	t.Run("tokens_roundtrip", func(t *testing.T) {
		tokens := map[string]string{"a": "1", "b": "2"}
		c.SetUserTokens(tokens)
		got := c.UserTokens()
		if got["a"] != "1" || got["b"] != "2" {
			t.Errorf("UserTokens = %v, want map[a:1 b:2]", got)
		}
	})

	t.Run("concurrent_writers", func(_ *testing.T) {
		var wg sync.WaitGroup
		for i := range 50 {
			wg.Go(func() {
				key := "conc-" + string(rune('A'+i%26))
				c.MarkProcessed(key)
				c.WasRecentlyProcessed(key)
			})
		}
		wg.Wait()
	})

	schedulerRunContract(t, c)
	checkAndMarkContract(t, c)
}

// schedulerRunContract exercises the scheduler-run portion of the api.Cache
// contract: the last-scheduler-run marker round-trips a whole-second value and
// resets to the zero time. Split out of RunCacheContract to keep that
// function's cognitive complexity under the gate.
func schedulerRunContract(t *testing.T, c api.Cache) {
	t.Helper()

	t.Run("scheduler_run_roundtrip", func(t *testing.T) {
		// Whole-second value: internal/cache persists the marker as a unix
		// int64 (time.Unix truncation), so the shared contract is pinned at
		// second granularity that both implementations honour.
		want := time.Unix(1700000000, 0)
		c.SetLastSchedulerRun(want)
		if got := c.LastSchedulerRun(); !got.Equal(want) {
			t.Errorf("LastSchedulerRun = %v, want %v", got, want)
		}
	})

	t.Run("scheduler_run_zero", func(t *testing.T) {
		c.SetLastSchedulerRun(time.Time{})
		if got := c.LastSchedulerRun(); !got.IsZero() {
			t.Errorf("LastSchedulerRun after zero set = %v, want zero", got)
		}
	})
}

// checkAndMarkContract exercises the atomic test-and-set portion of the
// api.Cache contract: CheckAndMark admits a fresh key exactly once and
// rejects it within the recent window. This is the TOCTOU-free idempotency
// gate scheduler.processRecentlyAddedEpisode relies on. Split out of
// RunCacheContract to keep that function's cognitive complexity under the
// gate.
func checkAndMarkContract(t *testing.T, c api.Cache) {
	t.Helper()

	t.Run("check_and_mark_admits_once", func(t *testing.T) {
		if !c.CheckAndMark("contract-cam-key") {
			t.Error("first CheckAndMark on a fresh key = false, want true (must admit and mark)")
		}
		if c.CheckAndMark("contract-cam-key") {
			t.Error("second CheckAndMark within the window = true, want false (already marked)")
		}
		if !c.WasRecentlyProcessed("contract-cam-key") {
			t.Error("after CheckAndMark, WasRecentlyProcessed should report the key processed")
		}
	})
}
