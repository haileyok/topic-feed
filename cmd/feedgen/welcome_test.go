package main

import (
	"testing"
	"time"
)

func TestWelcomeRecordIsBackdated(t *testing.T) {
	now := time.Date(2026, 10, 1, 12, 30, 0, 0, time.UTC)
	rec := welcomeRecord("hello", now, defaultDaysAgo)
	if rec["$type"] != "app.bsky.feed.post" || rec["text"] != "hello" {
		t.Errorf("record %v", rec)
	}
	created, err := time.Parse(time.RFC3339, rec["createdAt"].(string))
	if err != nil {
		t.Fatalf("createdAt %q: %v", rec["createdAt"], err)
	}
	if want := time.Date(2026, 7, 3, 12, 30, 0, 0, time.UTC); !created.Equal(want) {
		t.Errorf("dated %s, want 90 days before now: %s", created, want)
	}
	// A zero offset is plain "now", and the date is always written in UTC.
	if created, _ := time.Parse(time.RFC3339, welcomeRecord("x", now.In(time.FixedZone("x", -7*3600)), 0)["createdAt"].(string)); !created.Equal(now) {
		t.Errorf("0 days ago gave %s", created)
	}
	if got := welcomeRecord("x", now.In(time.FixedZone("x", 5*3600)), 1)["createdAt"].(string); got != "2026-09-30T12:30:00Z" {
		t.Errorf("createdAt %q is not UTC", got)
	}
}

func TestValidateDaysAgo(t *testing.T) {
	for _, n := range []int{0, 1, 90, maxDaysAgo} {
		if err := validateDaysAgo(n); err != nil {
			t.Errorf("%d: %v", n, err)
		}
	}
	for _, n := range []int{-1, maxDaysAgo + 1, 100000} {
		if err := validateDaysAgo(n); err == nil {
			t.Errorf("%d accepted", n)
		}
	}
}
