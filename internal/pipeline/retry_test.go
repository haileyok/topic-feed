package pipeline

import (
	"reflect"
	"testing"
	"time"
)

func TestRetryTargetsMatchSlotsToAttachments(t *testing.T) {
	// Four attachments; the second has alt text, so image_texts has slots for 1, 3, 4.
	ps := post{MediaCIDs: []string{"a", "b", "c", "d"}, MediaKinds: []string{"image", "image", "video", "image"},
		MediaAltTexts: []string{"", "a cat", "", ""}}
	got := retryTargets(ps, []string{SourceLLM, SourceError, SourceUnavailable}, 4)
	want := []retryTarget{{attachment: 2, slot: 1}, {attachment: 3, slot: 2}}
	if !reflect.DeepEqual(got, want) {
		t.Errorf("got %v, want %v", got, want)
	}
	if got := retryTargets(ps, []string{SourceNone, SourceBudget, SourceOCR}, 4); len(got) != 0 {
		t.Errorf("none/budget/ocr aren't retryable: %v", got)
	}
	if got := retryTargets(ps, []string{SourceError, SourceError, SourceError}, 1); !reflect.DeepEqual(got, []retryTarget{{0, 0}}) {
		t.Errorf("maxMedia 1: %v", got)
	}
}

func TestNextRetryState(t *testing.T) {
	now := time.Date(2026, 9, 30, 12, 0, 0, 0, time.UTC)
	cfg := DefaultRetry
	if s, a, _ := nextRetryState(2, true, false, now, cfg); s != RetryFixed || a != 2 {
		t.Errorf("fixed: %s %d", s, a)
	}
	// A failed try counts and backs off: after 1 failure wait 10m, after 2 wait 30m.
	if s, a, next := nextRetryState(0, false, false, now, cfg); s != RetryPending || a != 1 || next.Sub(now) != 10*time.Minute {
		t.Errorf("first failure: %s %d %v", s, a, next.Sub(now))
	}
	if _, a, next := nextRetryState(1, false, false, now, cfg); a != 2 || next.Sub(now) != 30*time.Minute {
		t.Errorf("second failure: %d %v", a, next.Sub(now))
	}
	// Paused tries don't count and come back soon.
	if s, a, next := nextRetryState(3, false, true, now, cfg); s != RetryPending || a != 3 || next.Sub(now) != 2*time.Minute {
		t.Errorf("paused: %s %d %v", s, a, next.Sub(now))
	}
	// The fifth failure gives up.
	if s, a, _ := nextRetryState(4, false, false, now, cfg); s != RetryGaveUp || a != 5 {
		t.Errorf("fifth failure: %s %d", s, a)
	}
}
