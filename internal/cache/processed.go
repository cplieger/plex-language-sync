package cache

import "time"

const (
	// recentlyProcessedWindow is the dedup horizon shared by
	// WasRecentlyProcessed and CheckAndMark.
	recentlyProcessedWindow = 5 * time.Minute
	// maxProcessedEntries is the soft cap that triggers an inline prune in
	// MarkProcessed and CheckAndMark to keep the map bounded.
	maxProcessedEntries = 10000
)

// WasRecentlyProcessed reports whether the given key was marked processed
// within the last 5 minutes. Used as a short-term dedup window for rapid
// successive webhook / websocket events on the same episode.
func (c *Cache) WasRecentlyProcessed(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	ts, ok := c.data.ProcessedEpisodes[key]
	if !ok {
		return false
	}
	return time.Since(time.Unix(ts, 0)) < recentlyProcessedWindow
}

// MarkProcessed records the current time against the key. Inline-prunes old
// entries when the map grows past 10000 entries to keep memory bounded.
func (c *Cache) MarkProcessed(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.ProcessedEpisodes == nil {
		c.data.ProcessedEpisodes = make(map[string]int64)
	}
	c.data.ProcessedEpisodes[key] = time.Now().Unix()
	// Inline prune if map grows too large (>10k entries).
	if len(c.data.ProcessedEpisodes) > maxProcessedEntries {
		c.pruneOldEntriesLocked()
	}
}

// CheckAndMark atomically tests and sets the recent-processed window for key:
// it returns true and records the current time when key was not processed
// within the last 5 minutes, or false (leaving the existing timestamp intact)
// when it was. The check and the mark happen under a single lock acquisition so
// two concurrent callers cannot both observe "not processed" for the same key —
// unlike a WasRecentlyProcessed-then-MarkProcessed sequence, which has a TOCTOU
// gap between its two separate lock acquisitions. Inline-prunes old entries when
// the map grows past 10000 entries.
func (c *Cache) CheckAndMark(key string) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.ProcessedEpisodes == nil {
		c.data.ProcessedEpisodes = make(map[string]int64)
	}
	if ts, ok := c.data.ProcessedEpisodes[key]; ok &&
		time.Since(time.Unix(ts, 0)) < recentlyProcessedWindow {
		return false
	}
	c.data.ProcessedEpisodes[key] = time.Now().Unix()
	if len(c.data.ProcessedEpisodes) > maxProcessedEntries {
		c.pruneOldEntriesLocked()
	}
	return true
}

// pruneOldEntriesLocked removes processed-episode entries older than 24h.
// Caller must hold c.mu.
func (c *Cache) pruneOldEntriesLocked() {
	cutoff := time.Now().Add(-24 * time.Hour).Unix()
	for k, ts := range c.data.ProcessedEpisodes {
		if ts < cutoff {
			delete(c.data.ProcessedEpisodes, k)
		}
	}
}
