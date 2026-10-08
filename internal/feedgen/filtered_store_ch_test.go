package feedgen

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/haileyok/topic-feed/internal/chdb"
)

// scratchFilteredStore is a store over a database of its own with the schema the filtered feeds read
// and write: post_pipeline (with predictions) and schema/015_filtered_feeds.sql.
func scratchFilteredStore(t *testing.T) *Store {
	t.Helper()
	if os.Getenv("TOPICFEED_CLICKHOUSE_TEST") == "" {
		t.Skip("set TOPICFEED_CLICKHOUSE_TEST=1 (and CLICKHOUSE_PASSWORD) to run against a ClickHouse server")
	}
	ctx := context.Background()
	admin, err := chdb.Open(ctx, chdb.FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	scratch := fmt.Sprintf("scratch_filtered_%d", time.Now().UnixNano())
	if err := admin.Exec(ctx, "CREATE DATABASE "+scratch); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := admin.Exec(context.Background(), "DROP DATABASE IF EXISTS "+scratch); err != nil {
			t.Errorf("dropping %s: %v", scratch, err)
		}
	})
	cfg := chdb.FromEnv()
	cfg.Database = scratch
	conn, err := chdb.Open(ctx, cfg)
	if err != nil {
		t.Fatal(err)
	}
	for _, file := range []string{"004_post_pipeline.sql", "005_predictions.sql", "015_filtered_feeds.sql", "015_filtered_feeds.sql"} {
		b, err := os.ReadFile("../../schema/" + file)
		if err != nil {
			t.Fatal(err)
		}
		for _, stmt := range strings.Split(stripSQLComments(string(b)), ";") {
			if lines := strings.TrimSpace(stmt); lines != "" {
				if err := conn.Exec(ctx, lines); err != nil {
					t.Fatalf("%s: %v\n%s", file, err, lines)
				}
			}
		}
	}
	return &Store{Conn: conn}
}

func stripSQLComments(s string) string {
	var out []string
	for _, l := range strings.Split(s, "\n") {
		if i := strings.Index(l, "--"); i >= 0 {
			l = l[:i]
		}
		out = append(out, l)
	}
	return strings.Join(out, "\n")
}

// TestScorePostsClickHouse checks ScorePosts against post_pipeline: recent posts are found while only
// the days since they were made are read, a post indexed again on another day gives its newest
// scores, a post the pipeline dropped counts as unscored, and a batch with a record key that isn't a
// TID reads every day.
func TestScorePostsClickHouse(t *testing.T) {
	s := scratchFilteredStore(t)
	ctx := context.Background()
	now := time.Now().UTC()
	recent, yesterday, reindexed, dropped := postAt(now.Add(-2*time.Hour)), postAt(now.Add(-30*time.Hour)),
		postAt(now.Add(-72*time.Hour)), postAt(now.Add(-time.Hour))
	insert := func(uri string, indexed, processed time.Time, model, top string) {
		t.Helper()
		if err := s.Conn.Exec(ctx, `INSERT INTO post_pipeline (uri, did, indexed_at, processed_at, feed_policy, model, path_probs, top_path)
			VALUES (?, 'did:plc:abc', ?, ?, 'ok', ?, map(?, 0.9), ?)`, uri, indexed, processed, model, top, top); err != nil {
			t.Fatal(err)
		}
	}
	insert(recent, now.Add(-2*time.Hour), now.Add(-2*time.Hour), "v5", "art/photography")
	insert(yesterday, now.Add(-30*time.Hour), now.Add(-30*time.Hour), "v5", "gaming/video_games")
	insert(reindexed, now.Add(-72*time.Hour), now.Add(-72*time.Hour), "v4", "sports/baseball")
	insert(reindexed, now.Add(-time.Hour), now.Add(-time.Hour), "v5", "sports/american_football")
	insert(dropped, now.Add(-time.Hour), now.Add(-time.Hour), "", "")

	check := func(name string, uris []string) {
		t.Helper()
		got, err := s.ScorePosts(ctx, uris)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		want := map[string]string{recent: "art/photography", yesterday: "gaming/video_games", reindexed: "sports/american_football"}
		for uri, top := range want {
			if p := got[uri]; !p.Scored || p.TopPath != top {
				t.Errorf("%s: %s got %+v, want top path %s", name, uri, p, top)
			}
		}
		if _, ok := got[dropped]; ok {
			t.Errorf("%s: the dropped post came back as scored", name)
		}
		if len(got) != len(want) {
			t.Errorf("%s: %d posts, want %d", name, len(got), len(want))
		}
	}
	uris := []string{recent, yesterday, reindexed, dropped}
	if _, ok := indexedSince(uris, now); !ok {
		t.Fatal("these posts should be read by date")
	}
	check("by date", uris)
	check("every day", append(uris, "at://did:plc:abc/app.bsky.feed.post/self"))
}

func TestLeftOutPostsClickHouse(t *testing.T) {
	s := scratchFilteredStore(t)
	ctx := context.Background()
	w := NewLeftOutWriter(s.Conn, nil)
	base := time.Now().UTC().Truncate(time.Millisecond)
	rows := []LeftOutRow{
		{ViewerDID: "did:plc:alice", Feed: "hot", URI: "at://x/app.bsky.feed.post/1", Reason: "topic", Name: "us_politics", Value: 0.9, Cutoff: 0.5, Bound: "max", LeftOutAt: base},
		{ViewerDID: "did:plc:alice", Feed: "hot", URI: "at://x/app.bsky.feed.post/2", Reason: "label", Name: "porn", LeftOutAt: base.Add(time.Second)},
		// Post 1 again, later, for another reason: it is shown once, with the newer reason.
		{ViewerDID: "did:plc:alice", Feed: "hot", URI: "at://x/app.bsky.feed.post/1", Reason: "signal", Name: "critical", Value: 0.8, Cutoff: 0.6, Bound: "max", Rule: "technology", LeftOutAt: base.Add(2 * time.Second)},
		{ViewerDID: "did:plc:bob", Feed: "hot", URI: "at://x/app.bsky.feed.post/3", Reason: "label", Name: "gore", LeftOutAt: base},
		{ViewerDID: "did:plc:alice", Feed: "other", URI: "at://x/app.bsky.feed.post/4", Reason: "unscored", LeftOutAt: base},
	}
	if err := chdb.Insert(ctx, s.Conn, w.Table, rows); err != nil {
		t.Fatal(err)
	}
	got, err := s.LeftOutPosts(ctx, "did:plc:alice", "hot", LeftOutCursor{}, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].URI != "at://x/app.bsky.feed.post/1" || got[1].URI != "at://x/app.bsky.feed.post/2" {
		t.Fatalf("got %+v", got)
	}
	want := LeftOutReason{Kind: "signal", Name: "critical", Value: 0.8, Cutoff: 0.6, Bound: "max", Rule: "technology"}
	if got[0].Reason != want || !got[0].At.Equal(base.Add(2*time.Second)) {
		t.Errorf("post 1: %+v at %v, want the newer reason %+v", got[0].Reason, got[0].At, want)
	}
	// Page by page: the second page starts after the first's last post, even with posts left out in
	// the same millisecond.
	first, _ := s.LeftOutPosts(ctx, "did:plc:alice", "hot", LeftOutCursor{}, 1)
	if len(first) != 1 || first[0].URI != "at://x/app.bsky.feed.post/1" {
		t.Fatalf("page 1: %+v", first)
	}
	second, _ := s.LeftOutPosts(ctx, "did:plc:alice", "hot", LeftOutCursor{At: first[0].At, URI: first[0].URI}, 1)
	if len(second) != 1 || second[0].URI != "at://x/app.bsky.feed.post/2" {
		t.Fatalf("page 2: %+v", second)
	}
	same := base.Add(10 * time.Second)
	var tied []LeftOutRow
	for i := 0; i < 5; i++ {
		tied = append(tied, LeftOutRow{ViewerDID: "did:plc:carol", Feed: "hot", URI: fmt.Sprintf("at://x/app.bsky.feed.post/t%d", i), Reason: "topic", LeftOutAt: same})
	}
	if err := chdb.Insert(ctx, s.Conn, w.Table, tied); err != nil {
		t.Fatal(err)
	}
	var after LeftOutCursor
	seen := map[string]bool{}
	for page := 0; page < 10; page++ {
		got, err := s.LeftOutPosts(ctx, "did:plc:carol", "hot", after, 2)
		if err != nil {
			t.Fatal(err)
		}
		for _, p := range got {
			if seen[p.URI] {
				t.Errorf("%s twice", p.URI)
			}
			seen[p.URI] = true
		}
		if len(got) < 2 {
			break
		}
		after = LeftOutCursor{At: got[len(got)-1].At, URI: got[len(got)-1].URI}
	}
	if len(seen) != 5 {
		t.Errorf("paged through %d of 5 posts left out in one millisecond", len(seen))
	}
}

func TestFilteredStoreKeepsLoginsAndFiltersClickHouse(t *testing.T) {
	s := scratchFilteredStore(t)
	ctx := context.Background()
	if got, err := s.LoadLogin(ctx, "did:plc:alice"); err != nil || got != nil {
		t.Fatalf("no login: %q %v", got, err)
	}
	s.SaveLogin(ctx, "did:plc:alice", []byte("sealed-1"))
	time.Sleep(5 * time.Millisecond)
	s.SaveLogin(ctx, "did:plc:alice", []byte("sealed-2"))
	if got, _ := s.LoadLogin(ctx, "did:plc:alice"); string(got) != "sealed-2" {
		t.Errorf("newest login: %q", got)
	}
	time.Sleep(5 * time.Millisecond)
	s.DeleteLogin(ctx, "did:plc:alice")
	if got, _ := s.LoadLogin(ctx, "did:plc:alice"); got != nil {
		t.Errorf("deleted login: %q", got)
	}

	fl := &Filters{Exclude: map[string]float32{"adult_content/explicit_posts": 0.4}}
	s.SaveViewerFilters(ctx, "did:plc:alice", "hot", fl)
	got, err := s.ViewerFilters(ctx, "did:plc:alice", "hot")
	if err != nil || got == nil || got.Exclude["adult_content/explicit_posts"] != 0.4 {
		t.Errorf("filters: %+v %v", got, err)
	}
	time.Sleep(5 * time.Millisecond)
	s.SaveViewerFilters(ctx, "did:plc:alice", "hot", nil)
	if got, _ := s.ViewerFilters(ctx, "did:plc:alice", "hot"); got != nil {
		t.Errorf("back to the feed's own: %+v", got)
	}
}
