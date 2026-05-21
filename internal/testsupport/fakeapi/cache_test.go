package fakeapi

import (
	"testing"
	"time"
)

func TestCacheRoundTrip(t *testing.T) {
	t.Parallel()
	c := NewCache()

	// Processed dedup.
	if c.WasRecentlyProcessed("k1") {
		t.Error("fresh cache should not report k1 as processed")
	}
	c.MarkProcessed("k1")
	if !c.WasRecentlyProcessed("k1") {
		t.Error("after MarkProcessed, WasRecentlyProcessed should be true")
	}

	// Language profile round-trip.
	c.LearnLanguageProfile("user1", "eng", "fra")
	if got, ok := c.SubtitleLangForAudio("user1", "eng"); !ok || got != "fra" {
		t.Errorf("SubtitleLangForAudio = (%q, %v), want (fra, true)", got, ok)
	}
	if _, ok := c.SubtitleLangForAudio("user1", "nope"); ok {
		t.Error("unknown audio lang should return ok=false")
	}

	// Empty audioLang is ignored (matches internal/cache).
	c.LearnLanguageProfile("user1", "", "fra")
	if _, ok := c.SubtitleLangForAudio("user1", ""); ok {
		t.Error("empty audio lang should not be stored")
	}

	// Token set/get round-trip with defensive copy.
	tokens := map[string]string{"u1": "t1", "u2": "t2"}
	c.SetUserTokens(tokens)
	got := c.UserTokens()
	if len(got) != 2 || got["u1"] != "t1" || got["u2"] != "t2" {
		t.Errorf("UserTokens = %v, want map[u1:t1 u2:t2]", got)
	}
	// Mutating the returned map must not affect cache state.
	got["u1"] = "mutated"
	if c.UserTokens()["u1"] != "t1" {
		t.Error("UserTokens should return a defensive copy")
	}
	// Mutating the input map after SetUserTokens must not affect cache.
	tokens["u1"] = "mutated"
	if c.UserTokens()["u1"] != "t1" {
		t.Error("SetUserTokens should defensive-copy its input")
	}

	// Scheduler run marker.
	if !c.LastSchedulerRun().IsZero() {
		t.Error("fresh cache should have zero LastSchedulerRun")
	}
	now := time.Now()
	c.SetLastSchedulerRun(now)
	if !c.LastSchedulerRun().Equal(now) {
		t.Errorf("LastSchedulerRun = %v, want %v", c.LastSchedulerRun(), now)
	}

	// Processed() is sorted.
	c.MarkProcessed("a")
	c.MarkProcessed("z")
	c.MarkProcessed("m")
	list := c.Processed()
	for i := 1; i < len(list); i++ {
		if list[i-1] > list[i] {
			t.Errorf("Processed() not sorted at index %d: %v", i, list)
		}
	}
}
