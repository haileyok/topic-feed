package pipeline

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/haileyok/topic-feed/internal/chdb"
)

// retrySchema creates, in a scratch database, the tables the retry worker reads and writes.
func retrySchema(t *testing.T, exec func(string)) {
	t.Helper()
	statements := func(file string) []string {
		b, err := os.ReadFile("../../schema/" + file)
		if err != nil {
			t.Fatal(err)
		}
		var lines []string
		for _, l := range strings.Split(string(b), "\n") {
			if i := strings.Index(l, "--"); i >= 0 {
				l = l[:i]
			}
			lines = append(lines, l)
		}
		var out []string
		for _, s := range strings.Split(strings.Join(lines, "\n"), ";") {
			if s = strings.TrimSpace(s); s != "" {
				out = append(out, s)
			}
		}
		return out
	}
	table := func(s string) string {
		f := strings.Fields(strings.NewReplacer("(", " ", "\n", " ").Replace(s))
		if len(f) > 5 && f[0] == "CREATE" && f[1] == "TABLE" {
			return f[5]
		}
		return ""
	}
	for _, s := range statements("001_init.sql") {
		switch table(s) {
		case "posts", "deletions", "account_status":
			exec(s)
		}
	}
	for _, file := range []string{"002_media_labels.sql", "004_post_pipeline.sql", "005_predictions.sql", "008_image_retry.sql", "013_post_pipeline_pictures.sql"} {
		for _, s := range statements(file) {
			if strings.HasPrefix(s, "ALTER TABLE posts ") || strings.HasPrefix(s, "ALTER TABLE post_pipeline ") ||
				table(s) == "post_pipeline" || table(s) == "image_retry_queue" {
				exec(s)
			}
		}
	}
}

type deletionRow struct {
	DID        string    `ch:"did"`
	Collection string    `ch:"collection"`
	RKey       string    `ch:"rkey"`
	URI        string    `ch:"uri"`
	IndexedAt  time.Time `ch:"indexed_at"`
}

type accountRow struct {
	DID       string    `ch:"did"`
	Active    uint8     `ch:"active"`
	Status    string    `ch:"status"`
	IndexedAt time.Time `ch:"indexed_at"`
}

// TestRetryLeavesGonePostsAlone runs the sweep and two retry rounds on a real server (scratch
// database, only when TOPICFEED_CLICKHOUSE_TEST is set): a post whose picture is missing is
// retried, but a deleted post and a post of an inactive account are never queued, and one that was
// deleted after it was queued is dropped from the queue without a download.
func TestRetryLeavesGonePostsAlone(t *testing.T) {
	if os.Getenv("TOPICFEED_CLICKHOUSE_TEST") == "" {
		t.Skip("set TOPICFEED_CLICKHOUSE_TEST=1 (and CLICKHOUSE_PASSWORD) to run against a ClickHouse server")
	}
	ctx := context.Background()
	admin, err := chdb.Open(ctx, chdb.FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	scratch := fmt.Sprintf("scratch_retry_%d", time.Now().UnixNano())
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
	retrySchema(t, func(s string) {
		t.Helper()
		if err := conn.Exec(ctx, s); err != nil {
			t.Fatalf("%v\n%s", err, s)
		}
	})

	at := time.Now().UTC()
	uri := func(name string) string { return "at://did:plc:" + name + "/app.bsky.feed.post/1" }
	// Six posts, each with one picture the pipeline could not download.
	names := []string{"live", "deleted", "inactive", "deletedlater", "reactivated", "inactivelater"}
	var posts []post
	var rows []Row
	for _, n := range names {
		posts = append(posts, post{URI: uri(n), DID: "did:plc:" + n, IndexedAt: at.Add(-time.Hour), MediaKinds: []string{"image"},
			MediaCIDs: []string{"bafy" + n}, MediaAlts: []string{}, MediaAltTexts: []string{}, Tags: []string{}, SelfLabels: []string{}})
		r := newRow(uri(n), "did:plc:"+n, at.Add(-time.Hour), "ok", nil)
		r.ProcessedAt, r.PicturesWanted, r.PicturesUsed = at.Add(-59*time.Minute), 1, 0
		rows = append(rows, r)
	}
	if err := chdb.Insert(ctx, conn, "posts", posts); err != nil {
		t.Fatal(err)
	}
	if err := chdb.Insert(ctx, conn, "post_pipeline", rows); err != nil {
		t.Fatal(err)
	}
	if err := chdb.Insert(ctx, conn, "deletions", []deletionRow{{DID: "did:plc:deleted", Collection: "app.bsky.feed.post", RKey: "1", URI: uri("deleted"), IndexedAt: at.Add(-30 * time.Minute)}}); err != nil {
		t.Fatal(err)
	}
	if err := chdb.Insert(ctx, conn, "account_status", []accountRow{
		{DID: "did:plc:inactive", Active: 0, Status: "takendown", IndexedAt: at.Add(-20 * time.Minute)},
		{DID: "did:plc:reactivated", Active: 0, Status: "deactivated", IndexedAt: at.Add(-40 * time.Minute)},
		{DID: "did:plc:reactivated", Active: 1, Status: "", IndexedAt: at.Add(-10 * time.Minute)}, // the later status wins
	}); err != nil {
		t.Fatal(err)
	}

	rt := &statusTransport{status: http.StatusNotFound}
	p := testPipeline(rt)
	p.Conn = conn
	rc := DefaultRetry

	errsFetch := counterValue(metricErrors.WithLabelValues(stageFetch))
	errsRetry := counterValue(metricErrors.WithLabelValues(stageRetryFetch))
	deletedBefore := counterValue(metricRetries.WithLabelValues(RetryGone))

	queue := func() map[string]queueRow {
		t.Helper()
		var qs []queueRow
		if err := conn.Select(ctx, &qs, `SELECT uri, did, indexed_at, status, attempts, next_attempt_at, last_error, queued_at, updated_at FROM image_retry_queue FINAL`); err != nil {
			t.Fatal(err)
		}
		out := map[string]queueRow{}
		for _, q := range qs {
			out[strings.TrimSuffix(strings.TrimPrefix(q.URI, "at://did:plc:"), "/app.bsky.feed.post/1")] = q
		}
		return out
	}

	// Round 1: the sweep queues the live post, the ones deleted or deactivated later and the
	// reactivated one; the deleted post and the post of the inactive account are not queued. Each
	// queued post is tried.
	if err := p.retryRound(ctx, rc); err != nil {
		t.Fatal(err)
	}
	q := queue()
	for _, n := range []string{"deleted", "inactive"} {
		if _, queued := q[n]; queued {
			t.Errorf("%s was queued: %+v", n, q[n])
		}
	}
	for _, n := range []string{"live", "deletedlater", "reactivated", "inactivelater"} {
		if q[n].Status != RetryPending || q[n].Attempts != 1 {
			t.Errorf("%s after round 1: %+v, want pending after 1 attempt", n, q[n])
		}
	}
	if got := rt.calls.Load(); got != 4 {
		t.Errorf("round 1 made %d downloads, want 4", got)
	}

	// Then one of the queued posts is deleted and the account of another is taken down, and all
	// four come due again.
	if err := chdb.Insert(ctx, conn, "deletions", []deletionRow{{DID: "did:plc:deletedlater", Collection: "app.bsky.feed.post", RKey: "1", URI: uri("deletedlater"), IndexedAt: time.Now().UTC()}}); err != nil {
		t.Fatal(err)
	}
	if err := chdb.Insert(ctx, conn, "account_status", []accountRow{{DID: "did:plc:inactivelater", Active: 0, Status: "suspended", IndexedAt: time.Now().UTC()}}); err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec(ctx, `INSERT INTO image_retry_queue
		SELECT uri, did, indexed_at, status, attempts, now64(3) - 60, last_error, queued_at, now64(3) FROM image_retry_queue FINAL`); err != nil {
		t.Fatal(err)
	}

	// Round 2: the deleted post and the one of the suspended account are dropped from the queue
	// without a download; the others are tried.
	if err := p.retryRound(ctx, rc); err != nil {
		t.Fatal(err)
	}
	q = queue()
	for _, n := range []string{"deletedlater", "inactivelater"} {
		if q[n].Status != RetryGone {
			t.Errorf("%s after round 2: %+v, want status %q", n, q[n], RetryGone)
		}
	}
	for _, n := range []string{"live", "reactivated"} {
		if q[n].Status != RetryPending || q[n].Attempts != 2 {
			t.Errorf("%s after round 2: %+v, want pending after 2 attempts", n, q[n])
		}
	}
	if got := rt.calls.Load(); got != 6 {
		t.Errorf("two rounds made %d downloads, want 6 (4 + 2: a gone post costs none)", got)
	}

	// The failures of retries are counted as retry errors, not as live fetch errors.
	if got := counterValue(metricErrors.WithLabelValues(stageRetryFetch)) - errsRetry; got != 6 {
		t.Errorf("retry_fetch errors grew by %v, want 6", got)
	}
	if got := counterValue(metricErrors.WithLabelValues(stageFetch)) - errsFetch; got != 0 {
		t.Errorf("fetch errors grew by %v, want 0", got)
	}
	if got := counterValue(metricRetries.WithLabelValues(RetryGone)) - deletedBefore; got != 2 {
		t.Errorf("deleted retries grew by %v, want 2", got)
	}
}
