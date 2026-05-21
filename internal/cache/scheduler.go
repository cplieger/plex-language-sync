package cache

import "time"

// LastSchedulerRun returns the timestamp of the last deep-analysis run.
// A zero time.Time indicates the scheduler has never run (fresh install
// or cache reset).
func (c *Cache) LastSchedulerRun() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.LastSchedulerRun == 0 {
		return time.Time{}
	}
	return time.Unix(c.data.LastSchedulerRun, 0)
}

// SetLastSchedulerRun records the supplied timestamp as the most recent
// scheduler run. Stored as a unix int64 on disk (contract item 7 —
// persisted schema is frozen).
func (c *Cache) SetLastSchedulerRun(t time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if t.IsZero() {
		c.data.LastSchedulerRun = 0
		return
	}
	c.data.LastSchedulerRun = t.Unix()
}
