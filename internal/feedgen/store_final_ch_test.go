package feedgen

import (
	"context"
	"fmt"
	"math"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"

	"github.com/haileyok/topic-feed/internal/chdb"
)

type finalTestRow struct {
	URI         string             `ch:"uri"`
	DID         string             `ch:"did"`
	IndexedAt   time.Time          `ch:"indexed_at"`
	ProcessedAt time.Time          `ch:"processed_at"`
	FeedPolicy  string             `ch:"feed_policy"`
	Labels      []string           `ch:"labels"`
	Model       string             `ch:"model"`
	PathProbs   map[string]float32 `ch:"path_probs"`
	Signals     map[string]float32 `ch:"signals"`
	Tone        map[string]float32 `ch:"tone"`
	TopPath     string             `ch:"top_path"`
	TopPathP    float32            `ch:"top_path_p"`
}

// finalSchemaStatements returns the statements of a schema file, without its comments.
func finalSchemaStatements(t *testing.T, file string) []string {
	t.Helper()
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

// TestPerPartitionFinalAgainstClickHouse runs the window scans that end in perPartitionFinal on a real
// server: versions of a post in one partition still collapse to the newest, a window scan skips the
// partitions outside the window (it reads fewer rows), and the one known exception (a post with a row
// on each of two days) is as documented. It only runs when TOPICFEED_CLICKHOUSE_TEST is set (plus the
// CLICKHOUSE_ADDR, CLICKHOUSE_USER and CLICKHOUSE_PASSWORD that reach a server); it works in a database
// of its own and drops it afterwards.
func TestPerPartitionFinalAgainstClickHouse(t *testing.T) {
	if os.Getenv("TOPICFEED_CLICKHOUSE_TEST") == "" {
		t.Skip("set TOPICFEED_CLICKHOUSE_TEST=1 (and CLICKHOUSE_PASSWORD) to run against a ClickHouse server")
	}
	ctx := context.Background()
	admin, err := chdb.Open(ctx, chdb.FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	scratch := fmt.Sprintf("scratch_final_%d", time.Now().UnixNano())
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
	exec := func(stmt string) {
		t.Helper()
		if err := conn.Exec(ctx, stmt); err != nil {
			t.Fatalf("%v\n%s", err, stmt)
		}
	}
	for _, s := range finalSchemaStatements(t, "004_post_pipeline.sql") {
		exec(s)
	}
	for _, s := range finalSchemaStatements(t, "005_predictions.sql") {
		if strings.HasPrefix(s, "ALTER TABLE post_pipeline") {
			exec(s)
		}
	}

	at := time.Now().UTC()
	const topic = "technology/ai"
	row := func(name string, indexedAgo, processedAgo time.Duration, model string) finalTestRow {
		return finalTestRow{URI: "at://did:plc:" + name + "/app.bsky.feed.post/1", DID: "did:plc:" + name,
			IndexedAt: at.Add(-indexedAgo), ProcessedAt: at.Add(-processedAgo), FeedPolicy: "ok", Labels: []string{}, Model: model,
			PathProbs: map[string]float32{topic: 0.9}, Signals: map[string]float32{}, Tone: map[string]float32{}, TopPath: topic, TopPathP: 0.9}
	}
	rows := []finalTestRow{
		// one post, classified twice on the same day: the later row must win
		row("same", time.Hour, 30*time.Minute, "v4"),
		row("same", time.Hour, 10*time.Minute, "v5"),
		// one post indexed again a day later: a row in each of two daily partitions, the earlier day's processed last
		row("two", time.Hour, 20*time.Minute, "today"),
		row("two", 25*time.Hour, 5*time.Minute, "yesterday"),
	}
	for i := 0; i < 60; i++ { // three days ago: outside the window the scans ask for
		rows = append(rows, row(fmt.Sprintf("old%02d", i), 72*time.Hour, 72*time.Hour, "v4"))
	}
	if err := chdb.Insert(ctx, conn, "post_pipeline", rows); err != nil {
		t.Fatal(err)
	}

	type result struct {
		URI   string `ch:"uri"`
		Model string `ch:"model"`
	}
	since := at.Add(-48 * time.Hour)
	scan := func(suffix string) ([]string, uint64) {
		t.Helper()
		var read uint64
		var got []result
		qctx := clickhouse.Context(ctx, clickhouse.WithProgress(func(p *clickhouse.Progress) { read += p.Rows }))
		if err := conn.Select(qctx, &got, `
			SELECT uri, model FROM post_pipeline FINAL
			WHERE indexed_at >= ? AND model != ''
			ORDER BY uri, indexed_at`+suffix, since); err != nil {
			t.Fatal(err)
		}
		out := make([]string, len(got))
		for i, r := range got {
			out[i] = strings.TrimPrefix(r.URI, "at://did:plc:") + "=" + r.Model
		}
		return out, read
	}

	whole, readWhole := scan("")
	perPartition, readPerPartition := scan(perPartitionFinal)

	if want := "same/app.bsky.feed.post/1=v5|two/app.bsky.feed.post/1=yesterday"; strings.Join(whole, "|") != want {
		t.Errorf("plain FINAL gave %v, want the newest row of each post", whole)
	}
	// Versions in one partition collapse to the newest; a post with a row on each of two days comes back
	// twice (Rank keeps one of them).
	if want := "same/app.bsky.feed.post/1=v5|two/app.bsky.feed.post/1=yesterday|two/app.bsky.feed.post/1=today"; strings.Join(perPartition, "|") != want {
		t.Errorf("per-partition FINAL gave %v, want\n%s", perPartition, want)
	}
	if readPerPartition >= readWhole {
		t.Errorf("per-partition FINAL read %d rows, plain FINAL %d: it should skip the partitions outside the window", readPerPartition, readWhole)
	}
	t.Logf("rows read: plain FINAL %d, per-partition FINAL %d", readWhole, readPerPartition)

	// Volumes: posts per hour over the last 3 hours, a post counted once however many versions it has.
	vol, err := (&Store{Conn: conn}).Volumes(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// "same" (one post, two versions) and the today row of "two": two posts in three hours.
	if got, want := vol[topic], 2.0/3; math.Abs(got-want) > 1e-9 {
		t.Errorf("volume of %s = %v per hour, want %v", topic, got, want)
	}
}
