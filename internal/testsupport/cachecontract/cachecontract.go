// Package cachecontract is the persisted-cache contract suite, run against
// both the real cache (internal/cache) and its in-memory fake
// (internal/testsupport/fakeapi), which keeps the fake honest: a fake that
// drifts from the real store turns every test built on it into a test of the
// fake. Import from _test.go files only.
package cachecontract

import (
	"context"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/cplieger/plex-language-sync/internal/streams"
)

// Contract is the FULL persisted surface, and the only wide interface in this
// app. Run is its one consumer, because the thing under test IS the whole
// surface; production consumers each declare the methods they use, at their
// own package.
type Contract interface {
	WasRecentlyProcessed(key string) bool
	MarkProcessed(key string)
	CheckAndMark(key string) bool
	LearnLanguageProfile(userID string, choice streams.LanguageChoice)
	SubtitleLangForAudio(userID, audioLang string) (string, bool)
	RecordIntent(userID, key string, intent *streams.Intent)
	IntentFor(userID, key string) (streams.Intent, bool)
	LatestIntentFor(exceptUser string, match func(user, key string, in streams.Intent) bool) (in streams.Intent, key, fromUser string, ok bool)
	AppliedFor(userID, ratingKey string) (streams.Applied, bool)
	RecordApplied(userID, ratingKey string, a streams.Applied)
	ForgetApplied(userID, ratingKey string)
	UserTokens() map[string]string
	SetUserTokens(tokens map[string]string)
}

// Run exercises the persisted-cache contract against c.
func Run(t *testing.T, c Contract) {
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
	appliedContract(t, c)

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
	// Last: a store that runs the matcher under its own lock deadlocks here,
	// and every later subtest would hang on that lock.
	latestIntentContract(t, c)
}

// Language-code literals shared by the contract subtests.
const (
	langJPN = "jpn"
	langENG = "eng"
	langFRA = "fra"
)

// intentContract exercises the intent-ledger portion of the Contract:
// record/read round-trip with deep-copy isolation and the nil-subtitle
// ("no subtitles") form. Edge behaviors live in intentEdgeContract.
func intentContract(t *testing.T, c Contract) {
	t.Helper()
	anchor42 := streams.Anchor{RatingKey: "1001", Season: 1, Index: 2, AudioID: 9001, SubtitleID: 9002}

	t.Run("intent_roundtrip", func(t *testing.T) {
		in := streams.NewIntent(streams.Pair{
			Audio:    &streams.Stream{LanguageCode: langJPN, Codec: "eac3", Channels: 6},
			Subtitle: &streams.Stream{LanguageCode: langENG, Codec: "ass", Forced: false},
		}, anchor42, 1700000000)
		c.RecordIntent("u1", "show-42", in)
		got, ok := c.IntentFor("u1", "show-42")
		if !ok {
			t.Fatal("IntentFor after RecordIntent = ok=false, want true")
		}
		if got.Audio.LanguageCode != langJPN || got.Subtitle == nil || got.Subtitle.LanguageCode != langENG {
			t.Errorf("IntentFor = %+v, want jpn audio + eng subtitle", got)
		}
		if got.Anchor != anchor42 {
			t.Errorf("IntentFor anchor = %+v, want %+v", got.Anchor, anchor42)
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
		c.RecordIntent("u1", "show-43", streams.NewIntent(streams.Pair{Audio: &streams.Stream{LanguageCode: langENG}, Subtitle: nil}, streams.Anchor{}, 1700000001))
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
		c.RecordIntent("u1", "show-44", streams.NewIntent(streams.Pair{Audio: &streams.Stream{LanguageCode: langJPN}, Subtitle: nil}, streams.Anchor{}, 1))
		c.RecordIntent("u1", "show-44", streams.NewIntent(streams.Pair{Audio: &streams.Stream{LanguageCode: langFRA}, Subtitle: nil}, streams.Anchor{}, 2))
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
		c.RecordIntent("", "show-42", streams.NewIntent(streams.Pair{Audio: &streams.Stream{LanguageCode: "x"}, Subtitle: nil}, streams.Anchor{}, 1))
		c.RecordIntent("u1", "", streams.NewIntent(streams.Pair{Audio: &streams.Stream{LanguageCode: "x"}, Subtitle: nil}, streams.Anchor{}, 1))
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
func intentPerShowContract(t *testing.T, c Contract) {
	t.Helper()

	t.Run("intent_second_show_keeps_the_first", func(t *testing.T) {
		// A store that reset its per-user map on every write would keep only
		// the newest show, and the loss would stay invisible until a user
		// played an episode of an older one.
		c.RecordIntent("u3", "show-70", streams.NewIntent(streams.Pair{Audio: &streams.Stream{LanguageCode: langJPN}, Subtitle: nil}, streams.Anchor{}, 70))
		c.RecordIntent("u3", "show-71", streams.NewIntent(streams.Pair{Audio: &streams.Stream{LanguageCode: langFRA}, Subtitle: nil}, streams.Anchor{}, 71))
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

// appliedContract exercises the per-user record of what the app left on an
// episode: round-trip and overwrite. Edge behaviors live in appliedEdgeContract.
func appliedContract(t *testing.T, c Contract) {
	t.Helper()
	first := streams.Applied{
		Basis: streams.Basis{Version: 1700000000, Source: streams.SourceIntent},
		At:    1700000100, PartID: 5001, AudioID: 9001, SubtitleID: 9002,
	}
	second := streams.Applied{
		Basis: streams.Basis{From: "u2", Version: 1700000200, Source: streams.SourceHousehold},
		At:    1700000300, PartID: 5002, AudioID: 9011,
	}

	t.Run("applied_roundtrip_and_overwrite", func(t *testing.T) {
		if _, ok := c.AppliedFor("u1", "2001"); ok {
			t.Fatal("AppliedFor on a fresh key = ok=true, want false")
		}
		c.RecordApplied("u1", "2001", first)
		if got, ok := c.AppliedFor("u1", "2001"); !ok || got != first {
			t.Errorf("AppliedFor after RecordApplied = (%+v, %v), want (%+v, true)", got, ok, first)
		}
		c.RecordApplied("u1", "2001", second)
		if got, ok := c.AppliedFor("u1", "2001"); !ok || got != second {
			t.Errorf("AppliedFor after a second RecordApplied = (%+v, %v), want (%+v, true)", got, ok, second)
		}
	})

	appliedEdgeContract(t, c, first, second)
}

// appliedEdgeContract covers forget, per-user isolation and the empty-key
// guards of the applied record.
func appliedEdgeContract(t *testing.T, c Contract, first, second streams.Applied) {
	t.Helper()

	t.Run("applied_per_user_and_forget", func(t *testing.T) {
		c.RecordApplied("u1", "2002", first)
		c.RecordApplied("u2", "2002", second)
		c.ForgetApplied("u1", "2002")
		if _, ok := c.AppliedFor("u1", "2002"); ok {
			t.Error("AppliedFor after ForgetApplied = ok=true, want false")
		}
		if got, ok := c.AppliedFor("u2", "2002"); !ok || got != second {
			t.Errorf("another user's record after ForgetApplied = (%+v, %v), want (%+v, true)", got, ok, second)
		}
		c.ForgetApplied("nobody", "2002")
	})

	t.Run("applied_empty_keys_ignored", func(t *testing.T) {
		c.RecordApplied("", "2003", first)
		c.RecordApplied("u1", "", first)
		if _, ok := c.AppliedFor("", "2003"); ok {
			t.Error("empty-user record was stored; RecordApplied must ignore empty keys")
		}
		if _, ok := c.AppliedFor("u1", ""); ok {
			t.Error("empty-key record was stored; RecordApplied must ignore empty keys")
		}
	})
}

// checkAndMarkContract exercises the atomic test-and-set portion of the
// Contract: CheckAndMark admits a fresh key exactly once and rejects it within
// the recent window, the idempotency gate the recently-added path relies on.
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

// latestCase records one intent for the latest-intent subtests.
func latestCase(c Contract, user, key string, observedAt int64, anchorKey string) {
	c.RecordIntent(user, key, streams.NewIntent(streams.Pair{
		Audio:    &streams.Stream{LanguageCode: langJPN},
		Subtitle: &streams.Stream{LanguageCode: langENG},
	}, streams.Anchor{RatingKey: anchorKey, Season: 1, Index: 1, AudioID: 9001}, observedAt))
}

// keyPrefix accepts the records one subtest wrote; the contract shares one
// store across subtests, so every search is confined to its own keys.
func keyPrefix(prefix string) func(string, string, streams.Intent) bool {
	return func(_, key string, _ streams.Intent) bool { return strings.HasPrefix(key, prefix) }
}

// latestIntentContract exercises the household search: the newest accepted
// record of another user, its tie rule, its copy, and its matcher contract.
func latestIntentContract(t *testing.T, c Contract) {
	t.Helper()

	t.Run("latest_newest_accepted_wins", func(t *testing.T) {
		latestCase(c, "l1", "latest-1/a", 100, "2001")
		latestCase(c, "l2", "latest-1/b", 200, "2002")
		latestCase(c, "l3", "latest-1/c", 300, "2003")
		in, key, from, ok := c.LatestIntentFor("nobody", keyPrefix("latest-1/"))
		if !ok || key != "latest-1/c" || from != "l3" || in.ObservedAt != 300 {
			t.Errorf("LatestIntentFor = (%d, %q, %q, %v), want (300, latest-1/c, l3, true)", in.ObservedAt, key, from, ok)
		}
		in, key, from, ok = c.LatestIntentFor("l3", keyPrefix("latest-1/"))
		if !ok || key != "latest-1/b" || from != "l2" || in.ObservedAt != 200 {
			t.Errorf("LatestIntentFor except the newest owner = (%d, %q, %q, %v), want (200, latest-1/b, l2, true)", in.ObservedAt, key, from, ok)
		}
	})

	t.Run("latest_tie_by_user_then_key", func(t *testing.T) {
		latestCase(c, "t2", "latest-2/a", 500, "2011")
		latestCase(c, "t1", "latest-2/b", 500, "2012")
		latestCase(c, "t1", "latest-2/c", 500, "2013")
		if _, key, from, ok := c.LatestIntentFor("nobody", keyPrefix("latest-2/")); !ok || from != "t1" || key != "latest-2/b" {
			t.Errorf("LatestIntentFor on a tie = (%q, %q, %v), want (latest-2/b, t1, true)", key, from, ok)
		}
		if _, key, from, ok := c.LatestIntentFor("t1", keyPrefix("latest-2/")); !ok || from != "t2" || key != "latest-2/a" {
			t.Errorf("LatestIntentFor on a tie except t1 = (%q, %q, %v), want (latest-2/a, t2, true)", key, from, ok)
		}
	})

	latestEdgeContract(t, c)
	latestMatcherContract(t, c)
}

// latestEdgeContract covers an empty result and the copy on return.
func latestEdgeContract(t *testing.T, c Contract) {
	t.Helper()

	t.Run("latest_none_accepted", func(t *testing.T) {
		never := func(string, string, streams.Intent) bool { return false }
		if in, key, from, ok := c.LatestIntentFor("nobody", never); ok || key != "" || from != "" || in.ObservedAt != 0 {
			t.Errorf("LatestIntentFor with nothing accepted = (%+v, %q, %q, %v), want zero values and false", in, key, from, ok)
		}
	})

	t.Run("latest_result_is_a_copy", func(t *testing.T) {
		latestCase(c, "k1", "latest-4", 700, "2021")
		in, _, _, ok := c.LatestIntentFor("nobody", keyPrefix("latest-4"))
		if !ok || in.Subtitle == nil {
			t.Fatalf("LatestIntentFor = (%+v, %v), want the recorded intent with a subtitle", in, ok)
		}
		in.Subtitle.LanguageCode = "MUTATED"
		if again, _ := c.IntentFor("k1", "latest-4"); again.Subtitle.LanguageCode != langENG {
			t.Error("mutating a LatestIntentFor result leaked into cache state; it must deep-copy")
		}
	})
}

// latestMatcherContract pins what the matcher sees and when it runs: it
// judges the owner and the intent itself, and it runs with no store lock held.
func latestMatcherContract(t *testing.T, c Contract) {
	t.Helper()

	t.Run("latest_matcher_sees_intent_and_owner", func(t *testing.T) {
		latestCase(c, "m1", "latest-3", 100, "2031")
		latestCase(c, "m2", "latest-3", 200, "2032")
		latestCase(c, "m3", "latest-3", 300, "reject")
		byAnchor := func(_, key string, in streams.Intent) bool {
			return key == "latest-3" && in.Anchor.RatingKey != "reject"
		}
		if in, _, from, ok := c.LatestIntentFor("nobody", byAnchor); !ok || from != "m2" || in.Anchor.RatingKey != "2032" {
			t.Errorf("LatestIntentFor rejecting by anchor = (%q, %q, %v), want m2's record anchored on 2032", in.Anchor.RatingKey, from, ok)
		}
		byOwner := func(user, key string, _ streams.Intent) bool {
			return key == "latest-3" && user != "m3" && user != "m2"
		}
		if _, _, from, ok := c.LatestIntentFor("nobody", byOwner); !ok || from != "m1" {
			t.Errorf("LatestIntentFor rejecting by owner = (%q, %v), want m1's record", from, ok)
		}
	})

	t.Run("latest_matcher_may_read_the_store", func(t *testing.T) {
		latestCase(c, "r1", "latest-5", 100, "2041")
		latestCase(c, "r2", "latest-5", 200, "2042")
		readsStore := func(user, key string, _ streams.Intent) bool {
			own, ok := c.IntentFor(user, key)
			return key == "latest-5" && ok && own.Anchor.RatingKey == "2041"
		}
		type result struct {
			from string
			ok   bool
		}
		done := make(chan result, 1)
		go func() {
			_, _, from, ok := c.LatestIntentFor("nobody", readsStore)
			done <- result{from, ok}
		}()
		ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
		defer cancel()
		select {
		case got := <-done:
			if !got.ok || got.from != "r1" {
				t.Errorf("LatestIntentFor with a store-reading matcher = (%q, %v), want r1's record", got.from, got.ok)
			}
		case <-ctx.Done():
			t.Fatal("LatestIntentFor did not return: the matcher must run with no store lock held")
		}
	})
}
