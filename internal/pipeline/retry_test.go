package pipeline

import (
	"testing"
	"time"
)

func TestNextRetryState(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	cfg := DefaultRetry
	if s, a, _ := nextRetryState(2, true, now, cfg); s != RetryFixed || a != 2 {
		t.Errorf("fixed: %s %d", s, a)
	}
	// A failed try counts and backs off: after 1 failure wait 10m, after 2 wait 30m.
	if s, a, next := nextRetryState(0, false, now, cfg); s != RetryPending || a != 1 || next.Sub(now) != 10*time.Minute {
		t.Errorf("first failure: %s %d %v", s, a, next.Sub(now))
	}
	if _, a, next := nextRetryState(1, false, now, cfg); a != 2 || next.Sub(now) != 30*time.Minute {
		t.Errorf("second failure: %d %v", a, next.Sub(now))
	}
	// The fifth failure gives up.
	if s, a, _ := nextRetryState(4, false, now, cfg); s != RetryGaveUp || a != 5 {
		t.Errorf("fifth failure: %s %d", s, a)
	}
}
