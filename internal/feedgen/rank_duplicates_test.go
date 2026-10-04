package feedgen

import (
	"fmt"
	"testing"
	"time"
)

// A post indexed again on another day has a row in each daily partition, and a window scan of
// post_pipeline FINAL with perPartitionFinal can return both. Rank must put it in the feed once.
func TestRankKeepsAPostWithTwoRowsOnce(t *testing.T) {
	at := time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)
	for _, gap := range []int{0, 10} {
		var ps []Post
		for i := 0; i < 12; i++ {
			ps = append(ps, Post{
				URI:       fmt.Sprintf("at://did:plc:a%02d/app.bsky.feed.post/1", i),
				DID:       fmt.Sprintf("did:plc:a%02d", i),
				IndexedAt: at.Add(-time.Duration(i) * time.Minute),
			})
		}
		ps[3].Likes = 40
		again := ps[3]
		again.Likes = 0                           // engagement is attached to one of the two rows
		again.IndexedAt = at.Add(-26 * time.Hour) // the row from the earlier day
		ps = append(ps, again)

		r := DefaultRanking
		r.AuthorGap = gap
		out := Rank(ps, Feed{Ranking: r}, at)

		if len(out) != 12 {
			t.Errorf("author_gap %d: %d posts from 13 rows of 12 posts, want 12", gap, len(out))
		}
		seen := map[string]int{}
		for _, p := range out {
			seen[p.URI]++
		}
		for uri, n := range seen {
			if n != 1 {
				t.Errorf("author_gap %d: %s is in the feed %d times", gap, uri, n)
			}
		}
	}
}
