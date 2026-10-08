package feedgen

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"
)

// countingScores scores the posts in scored and counts which posts it was asked about.
type countingScores struct {
	FilteredStore
	scored map[string]bool
	asked  []string
	fail   bool
}

func (s *countingScores) ScorePosts(_ context.Context, uris []string) (map[string]ScoredPost, error) {
	s.asked = append(s.asked, uris...)
	if s.fail {
		return nil, errors.New("down")
	}
	out := map[string]ScoredPost{}
	for _, u := range uris {
		if s.scored[u] {
			out[u] = ScoredPost{URI: u, Scored: true, TopPath: "art/photography"}
		}
	}
	return out, nil
}

func TestScoreCache(t *testing.T) {
	ctx := context.Background()
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	store := &countingScores{scored: map[string]bool{"a": true, "b": true}}
	c := NewScoreCache(store)
	c.now = func() time.Time { return now }

	got, err := c.ScorePosts(ctx, []string{"a", "b", "new"})
	if err != nil || len(got) != 2 || !got["a"].Scored || got["new"].Scored {
		t.Fatalf("first lookup: %v %v", got, err)
	}
	// Within the TTLs nothing is asked again, and the unscored post stays unscored.
	now = now.Add(20 * time.Second)
	got, _ = c.ScorePosts(ctx, []string{"a", "b", "new"})
	if len(store.asked) != 3 || len(got) != 2 {
		t.Fatalf("second lookup asked the store again (%v) or changed (%v)", store.asked, got)
	}
	// After the unscored TTL the unscored post is asked about again (it may be scored by now); the
	// scored ones are still kept.
	store.scored["new"] = true
	now = now.Add(20 * time.Second)
	got, _ = c.ScorePosts(ctx, []string{"a", "b", "new"})
	if !slices.Equal(store.asked[3:], []string{"new"}) || !got["new"].Scored {
		t.Fatalf("after 40s asked %v, got %v", store.asked[3:], got)
	}
	// After the TTL everything is asked again.
	now = now.Add(11 * time.Minute)
	c.ScorePosts(ctx, []string{"a"})
	if !slices.Equal(store.asked[4:], []string{"a"}) {
		t.Fatalf("after the TTL asked %v", store.asked[4:])
	}
	// An error is passed on and nothing is cached.
	store.fail = true
	if _, err := c.ScorePosts(ctx, []string{"zzz"}); err == nil {
		t.Fatal("no error from a failing store")
	}
	store.fail = false
	c.ScorePosts(ctx, []string{"zzz"})
	if n := slices.Index(store.asked, "zzz"); n < 0 || store.asked[len(store.asked)-1] != "zzz" || len(store.asked)-n != 2 {
		t.Fatalf("after an error the post wasn't asked again: %v", store.asked)
	}
}

func TestScoreCacheStaysUnderMax(t *testing.T) {
	ctx := context.Background()
	c := NewScoreCache(&countingScores{scored: map[string]bool{}})
	c.Max = 100
	for i := range 1000 {
		c.ScorePosts(ctx, []string{string(rune('a'+i%26)) + time.Duration(i).String()})
		if len(c.m) > c.Max {
			t.Fatalf("%d posts kept, max %d", len(c.m), c.Max)
		}
	}
}
