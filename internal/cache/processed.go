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

// recordProcessedLocked stamps key with the current time and inline-prunes old
// entries when the map grows past the soft cap. Caller must hold c.mu.
func (c *Cache) recordProcessedLocked(key string) {
	if c.data.ProcessedEpisodes == nil {
		c.data.ProcessedEpisodes = make(map[string]int64)
	}
	c.data.ProcessedEpisodes[key] = time.Now().Unix()
	if len(c.data.ProcessedEpisodes) > maxProcessedEntries {
		c.pruneOldEntriesLocked()
	}
}

// MarkProcessed records the current time against the key. Inline-prunes old
// entries when the map grows past 10000 entries to keep memory bounded.
func (c *Cache) MarkProcessed(key string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.recordProcessedLocked(key)
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
	if ts, ok := c.data.ProcessedEpisodes[key]; ok &&
		time.Since(time.Unix(ts, 0)) < recentlyProcessedWindow {
		return false
	}
	c.recordProcessedLocked(key)
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
