package fakeapi

import (
	"sync"
	"testing"
	"time"

	"github.com/cplieger/plex-language-sync/internal/api"
	"github.com/cplieger/plex-language-sync/internal/streams"
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

	intentContract(t, c)

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

// Language-code literals shared by the contract subtests.
const (
	langJPN = "jpn"
	langENG = "eng"
	langFRA = "fra"
)

// intentContract exercises the intent-ledger portion of the api.Cache
// contract: record/read round-trip with deep-copy isolation and the
// nil-subtitle ("no subtitles") form. Edge behaviors live in
// intentEdgeContract. Split out of RunCacheContract to keep cognitive
// complexity under the gate.
func intentContract(t *testing.T, c api.Cache) {
	t.Helper()

	t.Run("intent_roundtrip", func(t *testing.T) {
		in := streams.NewIntent(
			&streams.Stream{LanguageCode: langJPN, Codec: "eac3", Channels: 6},
			&streams.Stream{LanguageCode: langENG, Codec: "ass", Forced: false},
			1700000000,
		)
		c.RecordIntent("u1", "show-42", in)
		got, ok := c.IntentFor("u1", "show-42")
		if !ok {
			t.Fatal("IntentFor after RecordIntent = ok=false, want true")
		}
		if got.Audio.LanguageCode != langJPN || got.Subtitle == nil || got.Subtitle.LanguageCode != langENG {
			t.Errorf("IntentFor = %+v, want jpn audio + eng subtitle", got)
		}
		// Deep-copy isolation: mutating the returned intent must not
		// affect stored state.
		got.Subtitle.LanguageCode = "MUTATED"
		again, _ := c.IntentFor("u1", "show-42")
		if again.Subtitle.LanguageCode != langENG {
			t.Error("mutating a returned intent leaked into cache state; IntentFor must deep-copy")
		}
	})

	t.Run("intent_nil_subtitle_roundtrip", func(t *testing.T) {
		c.RecordIntent("u1", "show-43", streams.NewIntent(
			&streams.Stream{LanguageCode: langENG}, nil, 1700000001))
		got, ok := c.IntentFor("u1", "show-43")
		if !ok || got.Subtitle != nil {
			t.Errorf("IntentFor = (%+v, %v), want ok with nil Subtitle (no-subtitles intent)", got, ok)
		}
	})

	intentEdgeContract(t, c)
}

// intentEdgeContract covers the ledger's edges: replace-on-rerecord and
// the nil/empty-key guards.
func intentEdgeContract(t *testing.T, c api.Cache) {
	t.Helper()

	t.Run("intent_rerecord_replaces", func(t *testing.T) {
		c.RecordIntent("u1", "show-44", streams.NewIntent(
			&streams.Stream{LanguageCode: langJPN}, nil, 1))
		c.RecordIntent("u1", "show-44", streams.NewIntent(
			&streams.Stream{LanguageCode: langFRA}, nil, 2))
		got, _ := c.IntentFor("u1", "show-44")
		if got.Audio.LanguageCode != langFRA || got.ObservedAt != 2 {
			t.Errorf("re-record did not replace: %+v", got)
		}
	})

	t.Run("intent_missing_nil_and_empty_keys", func(t *testing.T) {
		if _, ok := c.IntentFor("nobody", "show-42"); ok {
			t.Error("IntentFor unknown user = ok=true, want false")
		}
		c.RecordIntent("", "show-42", streams.NewIntent(&streams.Stream{LanguageCode: "x"}, nil, 1))
		c.RecordIntent("u1", "", streams.NewIntent(&streams.Stream{LanguageCode: "x"}, nil, 1))
		c.RecordIntent("u1", "show-nil", nil)
		if _, ok := c.IntentFor("", "show-42"); ok {
			t.Error("empty-user intent was stored; RecordIntent must ignore empty keys")
		}
		if _, ok := c.IntentFor("u1", ""); ok {
			t.Error("empty-show intent was stored; RecordIntent must ignore empty keys")
		}
		if _, ok := c.IntentFor("u1", "show-nil"); ok {
			t.Error("nil intent was stored; RecordIntent must ignore nil")
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
