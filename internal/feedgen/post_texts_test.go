package feedgen

import (
	"context"
	"fmt"
	"os"
	"testing"
	"time"

	"github.com/haileyok/topic-feed/internal/chdb"
)

// TestPostTextsAgainstClickHouse runs the query on a real server, in a database of its own that
// is dropped afterwards. Like the tuning store's test it only runs when TOPICFEED_CLICKHOUSE_TEST
// is set. The posts table here has only the columns the query reads.
func TestPostTextsAgainstClickHouse(t *testing.T) {
	if os.Getenv("TOPICFEED_CLICKHOUSE_TEST") == "" {
		t.Skip("set TOPICFEED_CLICKHOUSE_TEST=1 (and CLICKHOUSE_PASSWORD) to run against a ClickHouse server")
	}
	ctx := context.Background()
	admin, err := chdb.Open(ctx, chdb.FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	scratch := fmt.Sprintf("scratch_texts_%d", time.Now().UnixNano())
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
	if err := conn.Exec(ctx, `CREATE TABLE posts (uri String, text String, indexed_at DateTime64(6, 'UTC'))
		ENGINE = ReplacingMergeTree(indexed_at) ORDER BY uri`); err != nil {
		t.Fatal(err)
	}
	at := time.Now().UTC()
	if err := chdb.Insert(ctx, conn, "posts", []struct {
		URI       string    `ch:"uri"`
		Text      string    `ch:"text"`
		IndexedAt time.Time `ch:"indexed_at"`
	}{
		{"at://did:plc:a/app.bsky.feed.post/1", "first", at},
		{"at://did:plc:b/app.bsky.feed.post/2", "second with 'quotes' and \\ slashes", at},
		{"at://did:plc:c/app.bsky.feed.post/3", "third", at},
	}); err != nil {
		t.Fatal(err)
	}
	store := &Store{Conn: conn}

	got, err := store.PostTexts(ctx, []string{"at://did:plc:a/app.bsky.feed.post/1", "at://did:plc:b/app.bsky.feed.post/2", "at://did:plc:z/app.bsky.feed.post/9"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got["at://did:plc:a/app.bsky.feed.post/1"] != "first" || got["at://did:plc:b/app.bsky.feed.post/2"] != "second with 'quotes' and \\ slashes" {
		t.Errorf("%v", got)
	}
	// What is asked for is a value, never part of the query.
	got, err = store.PostTexts(ctx, []string{"x' OR '1'='1", "') OR 1=1 --"})
	if err != nil || len(got) != 0 {
		t.Errorf("%v, %v", got, err)
	}
	if got, err := store.PostTexts(ctx, nil); err != nil || got == nil || len(got) != 0 {
		t.Errorf("nothing asked for: %v, %v", got, err)
	}
}
