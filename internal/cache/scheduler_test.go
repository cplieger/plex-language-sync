package cache

import (
	"testing"
	"time"
)

func TestLastSchedulerRunZeroReturnsZeroTime(t *testing.T) {
	t.Parallel()
	c := New()
	if got := c.LastSchedulerRun(); !got.IsZero() {
		t.Errorf("LastSchedulerRun() on fresh cache = %v, want zero time", got)
	}
}

func TestSetLastSchedulerRunRoundTrips(t *testing.T) {
	t.Parallel()
	c := New()
	want := time.Unix(1700000000, 0)
	c.SetLastSchedulerRun(want)
	if got := c.LastSchedulerRun(); !got.Equal(want) {
		t.Errorf("LastSchedulerRun() = %v, want %v", got, want)
	}
}

func TestSetLastSchedulerRunZeroClears(t *testing.T) {
	t.Parallel()
	c := New()
	c.SetLastSchedulerRun(time.Unix(1700000000, 0))
	c.SetLastSchedulerRun(time.Time{})
	if got := c.LastSchedulerRun(); !got.IsZero() {
		t.Errorf("LastSchedulerRun() after zero set = %v, want zero", got)
	}
}
