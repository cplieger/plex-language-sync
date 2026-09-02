package cache

import (
	"sync"
	"testing"

	"github.com/cplieger/plex-language-sync/internal/streams"
)

// Contract is the FULL persisted surface, and the only wide interface in this
// app. It exists for exactly one consumer — RunContract below — because the
// thing under test IS the whole surface; no production code depends on it, so
// it forces no consumer to accept methods it does not call. Production
// consumers each declare the 4, 3 or 2 methods they actually use, at their own
// package.
type Contract interface {
	WasRecentlyProcessed(key string) bool
	MarkProcessed(key string)
	CheckAndMark(key string) bool
	LearnLanguageProfile(userID string, choice streams.LanguageChoice)
	SubtitleLangForAudio(userID, audioLang string) (string, bool)
	RecordIntent(userID, showKey string, intent *streams.Intent)
	IntentFor(userID, showKey string) (streams.Intent, bool)
	UserTokens() map[string]string
	SetUserTokens(tokens map[string]string)
}

// RunContract exercises the persisted-cache contract against any
// implementation. Both *Cache and the in-memory test fake must pass, which is
// what keeps the fake honest: a fake that drifts from the real store turns
// every test built on it into a test of the fake.
//
// It lives in an ordinary .go file, not a _test.go, because its second caller
// is in a different package and a _test.go file is visible only to its own
// package's test binary.
func RunContract(t *testing.T, c Contract) {
	t.Helper()

	t.Run("SetGet_roundtrip", func(t *testing.T) {
		c.LearnLanguageProfile("u1", streams.LanguageChoice{Audio: "eng", Subtitle: "fra"})
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

	checkAndMarkContract(t, c)
}

// Language-code literals shared by the contract subtests.
const (
	langJPN = "jpn"
	langENG = "eng"
	langFRA = "fra"
)

// intentContract exercises the intent-ledger portion of the Contract
// contract: record/read round-trip with deep-copy isolation and the
// nil-subtitle ("no subtitles") form. Edge behaviors live in
// intentEdgeContract. Split out of RunCacheContract to keep cognitive
// complexity under the gate.
func intentContract(t *testing.T, c Contract) {
	t.Helper()

	t.Run("intent_roundtrip", func(t *testing.T) {
		in := streams.NewIntent(streams.Pair{
			Audio:    &streams.Stream{LanguageCode: langJPN, Codec: "eac3", Channels: 6},
			Subtitle: &streams.Stream{LanguageCode: langENG, Codec: "ass", Forced: false},
		}, 1700000000)
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
		c.RecordIntent("u1", "show-43", streams.NewIntent(streams.Pair{Audio: &streams.Stream{LanguageCode: langENG}, Subtitle: nil}, 1700000001))
		got, ok := c.IntentFor("u1", "show-43")
		if !ok || got.Subtitle != nil {
			t.Errorf("IntentFor = (%+v, %v), want ok with nil Subtitle (no-subtitles intent)", got, ok)
		}
	})

	intentEdgeContract(t, c)
}

// intentEdgeContract covers the ledger's edges: replace-on-rerecord, the
// per-show independence of two entries, and the nil/empty-key guards.
func intentEdgeContract(t *testing.T, c Contract) {
	t.Helper()

	t.Run("intent_rerecord_replaces", func(t *testing.T) {
		c.RecordIntent("u1", "show-44", streams.NewIntent(streams.Pair{Audio: &streams.Stream{LanguageCode: langJPN}, Subtitle: nil}, 1))
		c.RecordIntent("u1", "show-44", streams.NewIntent(streams.Pair{Audio: &streams.Stream{LanguageCode: langFRA}, Subtitle: nil}, 2))
		got, _ := c.IntentFor("u1", "show-44")
		if got.Audio.LanguageCode != langFRA || got.ObservedAt != 2 {
			t.Errorf("re-record did not replace: %+v", got)
		}
	})

	intentPerShowContract(t, c)

	t.Run("intent_missing_nil_and_empty_keys", func(t *testing.T) {
		if _, ok := c.IntentFor("nobody", "show-42"); ok {
			t.Error("IntentFor unknown user = ok=true, want false")
		}
		c.RecordIntent("", "show-42", streams.NewIntent(streams.Pair{Audio: &streams.Stream{LanguageCode: "x"}, Subtitle: nil}, 1))
		c.RecordIntent("u1", "", streams.NewIntent(streams.Pair{Audio: &streams.Stream{LanguageCode: "x"}, Subtitle: nil}, 1))
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

// intentPerShowContract covers the ledger's per-show independence: two shows
// recorded for one user are two entries, and neither write disturbs the other.
// Split out of intentEdgeContract to keep that function's cognitive complexity
// under the gate.
func intentPerShowContract(t *testing.T, c Contract) {
	t.Helper()

	t.Run("intent_second_show_keeps_the_first", func(t *testing.T) {
		// A store that reset its per-user map on every write would keep only
		// the newest show, and the loss would stay invisible until a user
		// played an episode of an older one.
		c.RecordIntent("u3", "show-70", streams.NewIntent(streams.Pair{Audio: &streams.Stream{LanguageCode: langJPN}, Subtitle: nil}, 70))
		c.RecordIntent("u3", "show-71", streams.NewIntent(streams.Pair{Audio: &streams.Stream{LanguageCode: langFRA}, Subtitle: nil}, 71))
		first, ok := c.IntentFor("u3", "show-70")
		if !ok {
			t.Fatal("IntentFor(u3, show-70) = ok=false after a second show was recorded, want the first show's intent")
		}
		if first.Audio.LanguageCode != langJPN {
			t.Errorf("IntentFor(u3, show-70) audio = %q, want %q", first.Audio.LanguageCode, langJPN)
		}
		second, ok := c.IntentFor("u3", "show-71")
		if !ok {
			t.Fatal("IntentFor(u3, show-71) = ok=false, want the second show's intent")
		}
		if second.Audio.LanguageCode != langFRA {
			t.Errorf("IntentFor(u3, show-71) audio = %q, want %q", second.Audio.LanguageCode, langFRA)
		}
	})
}

// checkAndMarkContract exercises the atomic test-and-set portion of the
// Contract: CheckAndMark admits a fresh key exactly once and
// rejects it within the recent window. This is the TOCTOU-free idempotency
// gate scheduler.processRecentlyAddedEpisode relies on. Split out of
// RunCacheContract to keep that function's cognitive complexity under the
// gate.
func checkAndMarkContract(t *testing.T, c Contract) {
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
