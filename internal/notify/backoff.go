package notify

import "time"

// Config holds the tunables for the reconnect loop. Production code uses
// DefaultConfig(); tests construct a Config with shrunk durations so the
// reconnect-loop assertions run fast without mutating package globals.
type Config struct {
	// MinBackoff is the initial delay between reconnect attempts and the
	// floor nextBackoff clamps to. Default: 1s.
	MinBackoff time.Duration

	// MaxBackoff is the ceiling for exponential growth of the reconnect
	// delay. Default: 30s.
	MaxBackoff time.Duration

	// StableThreshold is how long a connection must stay open before the
	// backoff is reset on the next reconnect (so a long-lived session
	// pays back accumulated backoff). Default: 1 minute.
	StableThreshold time.Duration

	// ReadIdleTimeout is the application-level backstop for a stuck
	// websocket read. The primary dead-connection detection is TCP
	// keepalive (configured on the dialer at 30s probe interval); the
	// read timeout exists only as a safety net for pathological cases
	// where the OS reports the connection alive but the server has
	// silently stopped sending. Default: 1 hour. Plex doesn't send
	// heartbeats and can legitimately be quiet for tens of minutes
	// during off-peak windows; a short timeout here only churns the
	// connection without improving correctness.
	ReadIdleTimeout time.Duration
}

// DefaultConfig returns the production values. Preserves behaviour of
// the original wsMinBackoff / wsMaxBackoff / wsStableThreshold package
// vars from main.go.
func DefaultConfig() Config {
	return Config{
		MinBackoff:      time.Second,
		MaxBackoff:      30 * time.Second,
		StableThreshold: time.Minute,
		ReadIdleTimeout: time.Hour,
	}
}

// nextBackoff returns the next reconnect delay. Doubles current up to
// maxB, clamps at minB from below. When stable==true (the prior
// connection held open long enough to be considered "good"), resets to
// minB so a long-lived session pays back accumulated backoff.
func nextBackoff(current, minB, maxB time.Duration, stable bool) time.Duration {
	if stable {
		return minB
	}
	return max(min(current*2, maxB), minB)
}
