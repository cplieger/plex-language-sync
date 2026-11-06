package cache

import (
	"cmp"
	"slices"
	"strings"

	"github.com/cplieger/plex-language-sync/internal/streams"
)

// maxAppliedEntries caps the applied section so Save never encodes a
// state.json its own WithMaxBytes refuses. Under MarshalIndent one entry with
// ten-digit ids and From set encodes to about 250 bytes, so 180,000 entries
// plus processed_episodes at maxProcessedEntries come to about 46.4 MB, under
// maxCacheSize (50 MiB). TestAppliedCapFitsMaxCacheSize pins the arithmetic.
const maxAppliedEntries = 180_000

// AppliedFor returns what the app last left on one episode for one user.
// ok=false when no record exists.
func (c *Cache) AppliedFor(userID, ratingKey string) (streams.Applied, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	a, ok := c.data.Applied[userID][ratingKey]
	return a, ok
}

// RecordApplied stores the app's write on one episode for one user,
// replacing any earlier record. Empty keys are ignored.
func (c *Cache) RecordApplied(userID, ratingKey string, a streams.Applied) {
	if userID == "" || ratingKey == "" {
		return
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	if c.data.Applied == nil {
		c.data.Applied = make(map[string]map[string]streams.Applied)
	}
	if c.data.Applied[userID] == nil {
		c.data.Applied[userID] = make(map[string]streams.Applied)
	}
	c.data.Applied[userID][ratingKey] = a
}

// ForgetApplied drops the record for one episode and user, if any.
func (c *Cache) ForgetApplied(userID, ratingKey string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	perUser, ok := c.data.Applied[userID]
	if !ok {
		return
	}
	delete(perUser, ratingKey)
	if len(perUser) == 0 {
		delete(c.data.Applied, userID)
	}
}

// appliedCount returns the number of records across every user.
func appliedCount(m map[string]map[string]streams.Applied) int {
	n := 0
	for _, perUser := range m {
		n += len(perUser)
	}
	return n
}

// evictOldestApplied deletes records from m, oldest At first, until at most
// limit remain. Ties go by (user, rating key) so the survivors do not depend
// on map iteration order.
func evictOldestApplied(m map[string]map[string]streams.Applied, limit int) {
	total := appliedCount(m)
	if total <= limit {
		return
	}
	type entry struct {
		user, key string
		at        int64
	}
	all := make([]entry, 0, total)
	for user, perUser := range m {
		for key, a := range perUser {
			all = append(all, entry{user: user, key: key, at: a.At})
		}
	}
	slices.SortFunc(all, func(a, b entry) int {
		return cmp.Or(cmp.Compare(a.at, b.at), strings.Compare(a.user, b.user), strings.Compare(a.key, b.key))
	})
	for _, e := range all[:total-limit] {
		delete(m[e.user], e.key)
		if len(m[e.user]) == 0 {
			delete(m, e.user)
		}
	}
}
