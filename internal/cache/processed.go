package cache

import "time"

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
	return time.Since(time.Unix(ts, 0)) < 5*time.Minute
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
	if len(c.data.ProcessedEpisodes) > 10000 {
		c.pruneOldEntriesLocked()
	}
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
