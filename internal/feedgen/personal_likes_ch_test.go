package feedgen

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/haileyok/topic-feed/internal/chdb"
)

// likeProfileJoin is how LikeProfile used to compute a viewer's interests and count, all in SQL with
// two joins against post_pipeline. TestLikeProfileClickHouse checks LikeProfile against it.
func likeProfileJoin(ctx context.Context, conn driver.Conn, did string, since, now time.Time, halfLife time.Duration) (map[string]float64, int, error) {
	react := []any{did, since, did, since}
	var mass []struct {
		Path string  `ch:"path"`
		Mass float64 `ch:"mass"`
	}
	args := append([]any{now, halfLife.Seconds()}, react...)
	args = append(args, react...)
	if err := conn.Select(ctx, &mass, `
		SELECT path, sum(prob * pow(0.5, greatest(0, dateDiff('second', l.liked_at, ?)) / ?)) AS mass
		FROM (SELECT subject_uri, max(indexed_at) AS liked_at FROM `+likesAndReposts+` GROUP BY subject_uri) AS l
		INNER JOIN (
			SELECT uri, path_probs FROM post_pipeline FINAL
			WHERE model != '' AND uri IN (SELECT subject_uri FROM `+likesAndReposts+`)
		) AS pp ON l.subject_uri = pp.uri
		ARRAY JOIN mapKeys(pp.path_probs) AS path, mapValues(pp.path_probs) AS prob
		GROUP BY path`, args...); err != nil {
		return nil, 0, err
	}
	out := map[string]float64{}
	for _, m := range mass {
		out[m.Path] = m.Mass
	}
	var n uint64
	args = append(append([]any{}, react...), react...)
	if err := conn.QueryRow(ctx, `
		SELECT count() FROM (SELECT DISTINCT subject_uri FROM `+likesAndReposts+`) AS l
		INNER JOIN (
			SELECT uri FROM post_pipeline FINAL
			WHERE model != '' AND uri IN (SELECT subject_uri FROM `+likesAndReposts+`)
		) AS pp ON l.subject_uri = pp.uri`, args...).Scan(&n); err != nil {
		return nil, 0, err
	}
	return out, int(n), nil
}

func sameMass(a, b map[string]float64) error {
	if len(a) != len(b) {
		return fmt.Errorf("%d subtopics vs %d", len(a), len(b))
	}
	for k, v := range a {
		if w, ok := b[k]; !ok || math.Abs(v-w) > 1e-6*math.Max(1, math.Abs(w)) {
			return fmt.Errorf("%s: %v vs %v", k, v, w)
		}
	}
	return nil
}

// TestLikeProfileClickHouse checks LikeProfile against the SQL it replaced, on a scratch database:
// likes and reposts of the same post (the latest counts), a like from before the period, a post
// that isn't classified, one the pipeline dropped, one processed twice, and another viewer's likes.
func TestLikeProfileClickHouse(t *testing.T) {
	if os.Getenv("TOPICFEED_CLICKHOUSE_TEST") == "" {
		t.Skip("set TOPICFEED_CLICKHOUSE_TEST=1 (and CLICKHOUSE_PASSWORD) to run against a ClickHouse server")
	}
	ctx := context.Background()
	admin, err := chdb.Open(ctx, chdb.FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	scratch := fmt.Sprintf("scratch_likes_%d", time.Now().UnixNano())
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
	exec := func(stmt string, args ...any) {
		t.Helper()
		if err := conn.Exec(ctx, stmt, args...); err != nil {
			t.Fatalf("%v\n%s", err, stmt)
		}
	}
	for _, s := range schemaStatements(t, "004_post_pipeline.sql") {
		exec(s)
	}
	for _, s := range schemaStatements(t, "005_predictions.sql") {
		if strings.HasPrefix(s, "ALTER TABLE post_pipeline") {
			exec(s)
		}
	}
	for _, s := range schemaStatements(t, "001_init.sql") {
		if strings.HasPrefix(s, "CREATE TABLE IF NOT EXISTS likes") || strings.HasPrefix(s, "CREATE TABLE IF NOT EXISTS reposts") {
			exec(s)
		}
	}

	now := time.Now().UTC().Truncate(time.Second)
	ago := func(d time.Duration) time.Time { return now.Add(-d) }
	const viewer, other = "did:plc:viewer", "did:plc:other"
	p := func(n int) string { return fmt.Sprintf("at://did:plc:author/app.bsky.feed.post/p%d", n) }
	classify := func(uri string, processed time.Time, model string, probs map[string]float32) {
		exec(`INSERT INTO post_pipeline (uri, did, indexed_at, processed_at, feed_policy, model, path_probs)
			VALUES (?, 'did:plc:author', ?, ?, 'ok', ?, ?)`, uri, ago(5*24*time.Hour), processed, model, probs)
	}
	react := func(table, actor, uri string, at time.Time) {
		exec(`INSERT INTO `+table+` (actor_did, rkey, subject_uri, created_at, indexed_at) VALUES (?, ?, ?, ?, ?)`,
			actor, fmt.Sprintf("r%d", at.UnixNano()), uri, at, at)
	}
	classify(p(1), ago(100*time.Hour), "v5", map[string]float32{"art/photography": 0.8, "art/painting": 0.1})
	classify(p(2), ago(100*time.Hour), "v4", map[string]float32{"sports/baseball": 0.5})
	classify(p(2), ago(10*time.Hour), "v5", map[string]float32{"sports/baseball": 0.9, "sports/american_football": 0.05})
	classify(p(4), ago(10*time.Hour), "", map[string]float32{"humor": 1})
	classify(p(5), ago(10*time.Hour), "v5", map[string]float32{"humor": 0.7})
	react("likes", viewer, p(1), ago(72*time.Hour))
	react("reposts", viewer, p(1), ago(2*time.Hour)) // the latest reaction counts
	react("likes", viewer, p(2), ago(30*time.Hour))
	react("likes", viewer, p(3), ago(time.Hour))       // not classified
	react("likes", viewer, p(4), ago(time.Hour))       // dropped by the pipeline
	react("likes", viewer, p(5), ago(40*24*time.Hour)) // before the period
	react("likes", other, p(5), ago(time.Hour))

	s := &Store{Conn: conn}
	since, halfLife := ago(30*24*time.Hour), 7*24*time.Hour
	got, err := s.LikeProfile(ctx, viewer, since, now, halfLife)
	if err != nil {
		t.Fatal(err)
	}
	wantMass, wantPosts, err := likeProfileJoin(ctx, conn, viewer, since, now, halfLife)
	if err != nil {
		t.Fatal(err)
	}
	if err := sameMass(got.Mass, wantMass); err != nil {
		t.Errorf("interests differ from the join: %v\n got %v\nwant %v", err, got.Mass, wantMass)
	}
	if got.Posts != wantPosts || got.Posts != 2 {
		t.Errorf("classified posts: got %d, the join %d, want 2", got.Posts, wantPosts)
	}
	for _, n := range []int{1, 2, 3, 4} {
		if _, ok := got.Liked[p(n)]; !ok {
			t.Errorf("post %d missing from Liked", n)
		}
	}
	if len(got.Liked) != 4 {
		t.Errorf("Liked has %d posts, want 4: %v", len(got.Liked), got.Liked)
	}
	// A viewer with no likes.
	if got, err := s.LikeProfile(ctx, "did:plc:nobody", since, now, halfLife); err != nil || len(got.Mass)+got.Posts+len(got.Liked) != 0 {
		t.Errorf("viewer without likes: %+v, %v", got, err)
	}
}
