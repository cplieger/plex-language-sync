package notify

import "time"

// Config holds the tunables for the reconnect loop. Production uses
// DefaultConfig(); tests shrink durations so assertions run fast without
// mutating package globals.
type Config struct {
	// MinBackoff is the initial delay and the floor nextBackoff clamps to.
	MinBackoff time.Duration

	// MaxBackoff is the ceiling for exponential growth.
	MaxBackoff time.Duration

	// StableThreshold is how long a connection must stay open before
	// backoff resets on the next reconnect.
	StableThreshold time.Duration

	// ReadIdleTimeout is a safety net for a stuck read when TCP
	// keepalive fails to detect it. Plex sends no heartbeats and can be
	// quiet for tens of minutes off-peak, so keep this generous.
	ReadIdleTimeout time.Duration
}

// DefaultConfig returns the production values.
func DefaultConfig() Config {
	return Config{
		MinBackoff:      time.Second,
		MaxBackoff:      30 * time.Second,
		StableThreshold: time.Minute,
		ReadIdleTimeout: time.Hour,
	}
}

// nextBackoff returns the next reconnect delay: doubles current up to
// maxB, clamped at minB. stable=true resets to minB.
func nextBackoff(current, minB, maxB time.Duration, stable bool) time.Duration {
	if stable {
		return minB
	}
	return max(min(current*2, maxB), minB)
}
