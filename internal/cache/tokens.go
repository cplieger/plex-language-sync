package cache

import "maps"

// UserTokens returns a defensive copy of the userID → accessToken map.
// Mutating the returned map does not affect cache state — callers must
// use SetUserTokens to persist changes.
func (c *Cache) UserTokens() map[string]string {
	c.mu.Lock()
	defer c.mu.Unlock()
	out := make(map[string]string, len(c.data.UserTokens))
	maps.Copy(out, c.data.UserTokens)
	return out
}

// SetUserTokens replaces the stored user-token map wholesale. The supplied
// map is defensive-copied so callers retain exclusive ownership of the
// original. Passing nil clears the map to an empty non-nil value.
func (c *Cache) SetUserTokens(tokens map[string]string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	next := make(map[string]string, len(tokens))
	maps.Copy(next, tokens)
	c.data.UserTokens = next
}
