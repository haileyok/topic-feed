package feedgen

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/haileyok/topic-feed/internal/chdb"
)

// TestTuningStoreAgainstClickHouse runs the store's SQL on a real server. It needs one, so it
// only runs when TOPICFEED_CLICKHOUSE_TEST is set (plus the CLICKHOUSE_ADDR, CLICKHOUSE_USER and
// CLICKHOUSE_PASSWORD that reach it). It works in a database of its own and drops it
// afterwards, so it never touches real data.
func TestTuningStoreAgainstClickHouse(t *testing.T) {
	if os.Getenv("TOPICFEED_CLICKHOUSE_TEST") == "" {
		t.Skip("set TOPICFEED_CLICKHOUSE_TEST=1 (and CLICKHOUSE_PASSWORD) to run against a ClickHouse server")
	}
	ctx := context.Background()
	admin, err := chdb.Open(ctx, chdb.FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	scratch := fmt.Sprintf("scratch_tuning_%d", time.Now().UnixNano())
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

	schema, err := os.ReadFile("../../schema/010_viewer_settings.sql")
	if err != nil {
		t.Fatal(err)
	}
	apply := func() {
		t.Helper()
		if err := conn.Exec(ctx, strings.TrimSpace(string(schema))); err != nil {
			t.Fatalf("applying the schema: %v", err)
		}
	}
	apply()
	// Old versions of a viewer's settings are only dropped when parts merge, which can be
	// any time: with merging off, every save stays a row of its own, as it does for a while
	// on a real server, and reading must still give the newest.
	if err := conn.Exec(ctx, "SYSTEM STOP MERGES viewer_settings"); err != nil {
		t.Logf("can't stop merges, so the test may pass by luck: %v", err)
	}
	store := &Store{Conn: conn}
	same := func(got, want Tuning) bool {
		g, _ := json.Marshal(got)
		w, _ := json.Marshal(want)
		return string(g) == string(w)
	}

	const alice, bob = "did:plc:alice", "did:plc:bob"
	if got, err := store.ViewerTuning(ctx, alice); err != nil || !got.IsZero() {
		t.Fatalf("a viewer with nothing saved: %+v, %v", got, err)
	}

	first := Tuning{Topics: map[string]float64{"technology/ai": 0, "animals_nature/cats": 2.5}, Freshness: FreshnessFresh,
		AuthorGap: ptr(0), HalfLifeDays: 3, HidePromo: true, ShowSeen: true, LookbackDays: 14, MinLikes: 3, Interests: 12, WindowHours: 12, MinTopicProb: 0.7,
		MaxServes: 1, ListSize: 80,
		Ranking: &RankingTuning{Gravity: ptr(0.0), FreshEvery: ptr(0), PromoPenalty: ptr(2.0), Like: ptr(1.5), Repost: ptr(0.0), Reply: ptr(3.0), Quote: ptr(4.0)},
		Tone:    Rules{Max: map[string]float32{"outraged": 0.4}, Weights: map[string]float64{"supportive": 1.5}},
		Signals: Rules{Min: map[string]float32{"substance": 0.25}, Weights: map[string]float64{"news": -1}}}
	if err := store.SaveTuning(ctx, alice, first); err != nil {
		t.Fatal(err)
	}
	got, err := store.ViewerTuning(ctx, alice)
	if err != nil || !same(got, first) {
		t.Fatalf("read back %+v (%v), want %+v", got, err, first)
	}
	if got.AuthorGap == nil || *got.AuthorGap != 0 {
		t.Errorf("an author gap of 0 (no gap) must survive the round trip: %v", got.AuthorGap)
	}

	// Saving again replaces it; saving nothing clears it.
	time.Sleep(5 * time.Millisecond)
	second := Tuning{Topics: map[string]float64{"sports/baseball": 1}}
	if err := store.SaveTuning(ctx, alice, second); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ViewerTuning(ctx, alice); err != nil || !same(got, second) {
		t.Fatalf("after saving again: %+v (%v), want %+v", got, err, second)
	}
	time.Sleep(5 * time.Millisecond)
	if err := store.SaveTuning(ctx, alice, Tuning{}); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ViewerTuning(ctx, alice); err != nil || !got.IsZero() {
		t.Fatalf("after clearing: %+v (%v)", got, err)
	}

	// Viewers don't see each other's, and a viewer can't be picked out by SQL in their DID.
	if err := store.SaveTuning(ctx, bob, first); err != nil {
		t.Fatal(err)
	}
	if got, _ := store.ViewerTuning(ctx, alice); !got.IsZero() {
		t.Errorf("alice sees %+v", got)
	}
	if got, err := store.ViewerTuning(ctx, "x' OR '1'='1"); err != nil || !got.IsZero() {
		t.Errorf("a DID that is SQL: %+v, %v", got, err)
	}

	// The newest settings are the ones saved last, not the ones in the newest row: ClickHouse
	// happens to read the newest part first, but that is not something to rely on. Here the
	// row inserted last has the older time.
	if err := conn.Exec(ctx, `INSERT INTO viewer_settings VALUES ('did:plc:order', '{"freshness":"fresh"}', now64(3) + INTERVAL 1 HOUR)`); err != nil {
		t.Fatal(err)
	}
	if err := conn.Exec(ctx, `INSERT INTO viewer_settings VALUES ('did:plc:order', '{"freshness":"popular"}', now64(3))`); err != nil {
		t.Fatal(err)
	}
	if got, err := store.ViewerTuning(ctx, "did:plc:order"); err != nil || got.Freshness != FreshnessFresh {
		t.Errorf("read %+v (%v): the settings with the latest time win, however the rows are stored", got, err)
	}

	// Applying the schema again changes nothing.
	apply()
	if got, err := store.ViewerTuning(ctx, bob); err != nil || !same(got, first) {
		t.Errorf("after applying the schema again: %+v (%v)", got, err)
	}

	// Something in the table that isn't a tuning is an error naming the viewer, not a panic.
	if err := conn.Exec(ctx, "INSERT INTO viewer_settings VALUES ('did:plc:broken', 'not json', now64(3))"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ViewerTuning(ctx, "did:plc:broken"); err == nil || !strings.Contains(err.Error(), "did:plc:broken") {
		t.Errorf("err %v", err)
	}

	// A new binary deployed before the schema: the log says which file to apply.
	if err := conn.Exec(ctx, "DROP TABLE viewer_settings"); err != nil {
		t.Fatal(err)
	}
	if _, err := store.ViewerTuning(ctx, alice); err == nil || !strings.Contains(err.Error(), "010_viewer_settings.sql") {
		t.Errorf("a missing table: %v", err)
	}
}
