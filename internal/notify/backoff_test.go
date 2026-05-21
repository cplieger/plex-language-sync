package notify

import (
	"testing"
	"time"

	"pgregory.net/rapid"
)

func TestNextBackoff_DoublesUpToMax(t *testing.T) {
	t.Parallel()
	const minB = 1 * time.Second
	const maxB = 30 * time.Second
	tests := []struct {
		current, want time.Duration
	}{
		{1 * time.Second, 2 * time.Second},
		{2 * time.Second, 4 * time.Second},
		{4 * time.Second, 8 * time.Second},
		{8 * time.Second, 16 * time.Second},
		{16 * time.Second, 30 * time.Second},
		{30 * time.Second, 30 * time.Second},
		{45 * time.Second, 30 * time.Second},
	}
	for _, tt := range tests {
		got := nextBackoff(tt.current, minB, maxB, false)
		if got != tt.want {
			t.Errorf("nextBackoff(%v, min=%v, max=%v, false) = %v, want %v",
				tt.current, minB, maxB, got, tt.want)
		}
	}
}

func TestNextBackoff_StableResetsToMin(t *testing.T) {
	t.Parallel()
	const minB = 1 * time.Second
	const maxB = 30 * time.Second
	if got := nextBackoff(30*time.Second, minB, maxB, true); got != minB {
		t.Errorf("nextBackoff(30s, stable=true) = %v, want 1s", got)
	}
	if got := nextBackoff(minB, minB, maxB, true); got != minB {
		t.Errorf("nextBackoff(1s, stable=true) = %v, want 1s", got)
	}
}

func TestNextBackoff_NeverBelowMin(t *testing.T) {
	t.Parallel()
	const minB = 1 * time.Second
	const maxB = 30 * time.Second
	if got := nextBackoff(100*time.Millisecond, minB, maxB, false); got < minB {
		t.Errorf("nextBackoff(100ms, false) = %v, want >= 1s", got)
	}
	if got := nextBackoff(0, minB, maxB, false); got < minB {
		t.Errorf("nextBackoff(0, false) = %v, want >= 1s", got)
	}
}

func TestNextBackoff_PBTBounded(t *testing.T) {
	rapid.Check(t, func(t *rapid.T) {
		minMs := rapid.IntRange(1, 5000).Draw(t, "min_ms")
		maxMs := rapid.IntRange(minMs, 60000).Draw(t, "max_ms")
		curMs := rapid.IntRange(0, 120000).Draw(t, "cur_ms")
		minB := time.Duration(minMs) * time.Millisecond
		maxB := time.Duration(maxMs) * time.Millisecond
		cur := time.Duration(curMs) * time.Millisecond

		got := nextBackoff(cur, minB, maxB, false)
		if got < minB {
			t.Errorf("nextBackoff = %v below floor %v", got, minB)
		}
		if got > maxB {
			t.Errorf("nextBackoff = %v above ceiling %v", got, maxB)
		}

		gotStable := nextBackoff(cur, minB, maxB, true)
		if gotStable != minB {
			t.Errorf("nextBackoff(stable=true) = %v, want %v", gotStable, minB)
		}
	})
}

// TestDefaultConfig pins the production values so an accidental edit
// to DefaultConfig that would change the Loki alert timing shows up
// as a test failure.
func TestDefaultConfig(t *testing.T) {
	t.Parallel()
	cfg := DefaultConfig()
	if cfg.MinBackoff != time.Second {
		t.Errorf("DefaultConfig().MinBackoff = %v, want 1s", cfg.MinBackoff)
	}
	if cfg.MaxBackoff != 30*time.Second {
		t.Errorf("DefaultConfig().MaxBackoff = %v, want 30s", cfg.MaxBackoff)
	}
	if cfg.StableThreshold != time.Minute {
		t.Errorf("DefaultConfig().StableThreshold = %v, want 1m", cfg.StableThreshold)
	}
	if cfg.ReadIdleTimeout != time.Hour {
		t.Errorf("DefaultConfig().ReadIdleTimeout = %v, want 1h", cfg.ReadIdleTimeout)
	}
}
