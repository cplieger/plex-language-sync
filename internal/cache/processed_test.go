package cache

import (
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestCacheWasRecentlyProcessed(t *testing.T) {
	c := New()

	if c.WasRecentlyProcessed("ep1") {
		t.Error("expected false for unknown key")
	}

	c.MarkProcessed("ep1")
	if !c.WasRecentlyProcessed("ep1") {
		t.Error("expected true after marking")
	}
}

func TestCachePruneOldEntries(t *testing.T) {
	var c Cache
	c.data.ProcessedEpisodes = map[string]int64{
		"old": time.Now().Add(-48 * time.Hour).Unix(),
		"new": time.Now().Unix(),
	}
	c.pruneOldEntriesLocked()
	if _, ok := c.data.ProcessedEpisodes["old"]; ok {
		t.Error("old entry should be pruned")
	}
	if _, ok := c.data.ProcessedEpisodes["new"]; !ok {
		t.Error("new entry should be kept")
	}
}

func TestCacheMarkProcessedAutoprune(t *testing.T) {
	c := New()
	// Fill with >10000 old entries to trigger inline prune.
	old := time.Now().Add(-48 * time.Hour).Unix()
	for i := range 10001 {
		c.data.ProcessedEpisodes[fmt.Sprintf("ep%d", i)] = old
	}
	c.MarkProcessed("fresh")
	// After prune, old entries should be gone.
	if len(c.data.ProcessedEpisodes) > 2 {
		t.Errorf("expected pruned map, got %d entries", len(c.data.ProcessedEpisodes))
	}
}

func TestCacheMarkProcessedBoundary10000(t *testing.T) {
	c := New()
	// Fill with exactly 9999 old entries. After inserting "fresh", total
	// = 10000. The threshold is > 10000 (not >=), so prune should NOT
	// fire at exactly 10000.
	old := time.Now().Add(-48 * time.Hour).Unix()
	for i := range 9999 {
		c.data.ProcessedEpisodes[fmt.Sprintf("ep%d", i)] = old
	}
	c.MarkProcessed("fresh")
	// 9999 old + 1 fresh = 10000 entries. 10000 > 10000 is false → no prune.
	if len(c.data.ProcessedEpisodes) != 10000 {
		t.Errorf("MarkProcessed at exactly 10000 entries should NOT prune, got %d entries",
			len(c.data.ProcessedEpisodes))
	}
}

func TestCacheMarkProcessedNilMap(t *testing.T) {
	var c Cache
	// Don't initialize ProcessedEpisodes — test nil map path.
	c.MarkProcessed("test-key")
	if !c.WasRecentlyProcessed("test-key") {
		t.Error("expected true after MarkProcessed on nil map")
	}
}

func TestCacheWasRecentlyProcessedBoundary(t *testing.T) {
	t.Parallel()
	c := New()

	// Entry exactly at the 5-minute boundary should NOT be recent.
	c.data.ProcessedEpisodes["old"] = time.Now().Add(-5 * time.Minute).Unix()
	if c.WasRecentlyProcessed("old") {
		t.Error("WasRecentlyProcessed(old) = true, want false for entry exactly 5 min ago")
	}

	// Entry 4m59s ago should still be recent.
	c.data.ProcessedEpisodes["recent"] = time.Now().Add(-4*time.Minute - 59*time.Second).Unix()
	if !c.WasRecentlyProcessed("recent") {
		t.Error("WasRecentlyProcessed(recent) = false, want true for entry 4m59s ago")
	}

	// Entry just now should be recent.
	c.data.ProcessedEpisodes["now"] = time.Now().Unix()
	if !c.WasRecentlyProcessed("now") {
		t.Error("WasRecentlyProcessed(now) = false, want true for entry just now")
	}

	// Entry 6 minutes ago should not be recent.
	c.data.ProcessedEpisodes["stale"] = time.Now().Add(-6 * time.Minute).Unix()
	if c.WasRecentlyProcessed("stale") {
		t.Error("WasRecentlyProcessed(stale) = true, want false for entry 6 min ago")
	}
}

func TestCacheMarkProcessedPruneBoundary(t *testing.T) {
	t.Parallel()
	c := New()

	// Fill exactly 10000 entries — should NOT trigger prune.
	for i := range 10000 {
		c.data.ProcessedEpisodes[fmt.Sprintf("key-%d", i)] = time.Now().Unix()
	}
	c.MarkProcessed("trigger")
	// After adding one more (10001 total), prune should have run.
	// Since all entries are recent, none should be pruned.
	if len(c.data.ProcessedEpisodes) != 10001 {
		t.Errorf("after MarkProcessed with 10001 entries, got %d entries, want 10001",
			len(c.data.ProcessedEpisodes))
	}

	// Now add old entries to make prune effective.
	oldTS := time.Now().Add(-25 * time.Hour).Unix()
	for i := range 5000 {
		c.data.ProcessedEpisodes[fmt.Sprintf("old-%d", i)] = oldTS
	}
	// Total is now 15001. MarkProcessed triggers prune (>10000).
	c.MarkProcessed("trigger2")
	// Old entries should be pruned.
	if len(c.data.ProcessedEpisodes) > 10002 {
		t.Errorf("after prune, got %d entries, want ≤10002 (old entries removed)",
			len(c.data.ProcessedEpisodes))
	}
}

func TestCachePruneOldEntriesBoundary(t *testing.T) {
	t.Parallel()
	c := New()

	now := time.Now()
	// Entry exactly 24h ago — should NOT be pruned (cutoff is -24h, ts <
	// cutoff means strictly older).
	c.data.ProcessedEpisodes["exact-24h"] = now.Add(-24 * time.Hour).Unix()
	// Entry 23h59m ago — should NOT be pruned.
	c.data.ProcessedEpisodes["23h59m"] = now.Add(-23*time.Hour - 59*time.Minute).Unix()
	// Entry 25h ago — should be pruned.
	c.data.ProcessedEpisodes["25h"] = now.Add(-25 * time.Hour).Unix()
	// Entry just now — should NOT be pruned.
	c.data.ProcessedEpisodes["now"] = now.Unix()

	c.pruneOldEntriesLocked()

	if _, ok := c.data.ProcessedEpisodes["exact-24h"]; !ok {
		t.Error("entry at exactly 24h should NOT be pruned (boundary: ts == cutoff)")
	}
	if _, ok := c.data.ProcessedEpisodes["23h59m"]; !ok {
		t.Error("entry at 23h59m should NOT be pruned")
	}
	if _, ok := c.data.ProcessedEpisodes["25h"]; ok {
		t.Error("entry at 25h should be pruned")
	}
	if _, ok := c.data.ProcessedEpisodes["now"]; !ok {
		t.Error("entry at now should NOT be pruned")
	}
}

func TestCacheCheckAndMark(t *testing.T) {
	t.Run("fresh key returns true and marks", func(t *testing.T) {
		c := New()
		if !c.CheckAndMark("ep1") {
			t.Fatal("CheckAndMark(fresh) = false, want true")
		}
		if !c.WasRecentlyProcessed("ep1") {
			t.Error("after CheckAndMark returned true, WasRecentlyProcessed = false, want true")
		}
	})
	t.Run("second call within window returns false", func(t *testing.T) {
		c := New()
		c.CheckAndMark("ep1")
		if c.CheckAndMark("ep1") {
			t.Error("CheckAndMark(recent) = true, want false")
		}
	})
	t.Run("nil map path returns true", func(t *testing.T) {
		var c Cache
		if !c.CheckAndMark("ep1") {
			t.Error("CheckAndMark on zero-value Cache = false, want true")
		}
		if !c.WasRecentlyProcessed("ep1") {
			t.Error("zero-value Cache did not record the key")
		}
	})
	t.Run("entry older than 5m returns true and re-marks", func(t *testing.T) {
		c := New()
		c.data.ProcessedEpisodes["ep1"] = time.Now().Add(-6 * time.Minute).Unix()
		if !c.CheckAndMark("ep1") {
			t.Error("CheckAndMark(6m-old) = false, want true")
		}
	})
	t.Run("false return leaves the existing timestamp intact", func(t *testing.T) {
		c := New()
		ts := time.Now().Add(-1 * time.Minute).Unix()
		c.data.ProcessedEpisodes["ep1"] = ts
		if c.CheckAndMark("ep1") {
			t.Fatal("CheckAndMark(1m-old) = true, want false")
		}
		if got := c.data.ProcessedEpisodes["ep1"]; got != ts {
			t.Errorf("timestamp changed on false return: got %d, want %d (unchanged)", got, ts)
		}
	})
}

func TestCacheCheckAndMarkAtomic(t *testing.T) {
	c := New()
	const goroutines = 200
	var winners atomic.Int64
	var wg sync.WaitGroup
	wg.Add(goroutines)
	for range goroutines {
		go func() {
			defer wg.Done()
			if c.CheckAndMark("contended-key") {
				winners.Add(1)
			}
		}()
	}
	wg.Wait()
	if got := winners.Load(); got != 1 {
		t.Errorf("concurrent CheckAndMark winners = %d, want exactly 1 "+
			"(atomic test-and-set must let only one caller observe the key as unprocessed)", got)
	}
}

func TestCacheCheckAndMarkAutoprune(t *testing.T) {
	c := New()
	// Fill with >10000 old entries so the post-mark length check trips the
	// inline prune branch (the > maxProcessedEntries guard in CheckAndMark,
	// the twin of MarkProcessed's).
	old := time.Now().Add(-48 * time.Hour).Unix()
	for i := range 10001 {
		c.data.ProcessedEpisodes[fmt.Sprintf("ep%d", i)] = old
	}
	if !c.CheckAndMark("fresh") {
		t.Fatal("CheckAndMark(fresh) = false, want true")
	}
	// After prune, only recent entries ("fresh") survive; the 10001 old
	// entries are gone. A regression that dropped the prune call would leave
	// 10002 entries here.
	if len(c.data.ProcessedEpisodes) > 2 {
		t.Errorf("CheckAndMark did not inline-prune past the cap, got %d entries", len(c.data.ProcessedEpisodes))
	}
}
