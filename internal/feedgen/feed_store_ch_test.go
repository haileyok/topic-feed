package feedgen

import (
	"context"
	"fmt"
	"os"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/haileyok/topic-feed/internal/chdb"
)

// scratchFeedStore is a store over a database of its own with schema/014_user_feeds.sql applied, and
// merges stopped: every save stays a row of its own, as it does for a while on a real server.
func scratchFeedStore(t *testing.T) (*Store, func()) {
	t.Helper()
	if os.Getenv("TOPICFEED_CLICKHOUSE_TEST") == "" {
		t.Skip("set TOPICFEED_CLICKHOUSE_TEST=1 (and CLICKHOUSE_PASSWORD) to run against a ClickHouse server")
	}
	ctx := context.Background()
	admin, err := chdb.Open(ctx, chdb.FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	scratch := fmt.Sprintf("scratch_feeds_%d", time.Now().UnixNano())
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
	schema, err := os.ReadFile("../../schema/014_user_feeds.sql")
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
	apply() // idempotent: `make schema` runs it on every start
	if err := conn.Exec(ctx, "SYSTEM STOP MERGES user_feeds"); err != nil {
		t.Logf("can't stop merges, so the test may pass by luck: %v", err)
	}
	return &Store{Conn: conn}, apply
}

func rkeys(fs []StoredFeed) string {
	var out []string
	for _, f := range fs {
		out = append(out, f.Owner+"/"+f.Rkey)
	}
	return strings.Join(out, " ")
}

func catSpec(name string) FeedSpec {
	return FeedSpec{DisplayName: name, Description: "d", Paths: []string{"animals_nature/cats"}, Exclude: map[string]float32{"adult_content": 0.2},
		MinProb: 0.6, Ranking: Ranking{Weights: Weights{Like: 1, Repost: 2, Reply: 2, Quote: 3}, Gravity: 2.5, FreshEvery: 3, AuthorGap: 7, PromoPenalty: 0.5},
		AcceptsInteractions: true, Tone: Rules{Max: map[string]float32{"outraged": 0.4}, Weights: map[string]float64{"supportive": 1.5}},
		Signals: Rules{Min: map[string]float32{"substance": 0.25}}, MaxPosts: 1234}
}

// TestFeedStoreAgainstClickHouse runs the store's SQL on a real server. It needs one, so it only runs
// when TOPICFEED_CLICKHOUSE_TEST is set; it works in a database of its own and drops it afterwards.
func TestFeedStoreAgainstClickHouse(t *testing.T) {
	store, _ := scratchFeedStore(t)
	ctx := context.Background()
	const alice, bob = "did:plc:alice", "did:plc:bob"

	if got, err := store.ListFeeds(ctx); err != nil || len(got) != 0 {
		t.Fatalf("no feeds yet: %v, %v", got, err)
	}

	// A feed is stored whole, and comes back whole.
	if err := store.SaveFeed(ctx, StoredFeed{Owner: bob, Rkey: "cats", Spec: catSpec("Cats")}); err != nil {
		t.Fatal(err)
	}
	got, err := store.OwnerFeeds(ctx, bob)
	if err != nil || len(got) != 1 {
		t.Fatalf("%v, %v", got, err)
	}
	if !reflect.DeepEqual(got[0].Spec, catSpec("Cats")) || got[0].Owner != bob || got[0].Rkey != "cats" || got[0].CreatedAt.IsZero() {
		t.Errorf("not what was stored: %+v", got[0])
	}
	created := got[0].CreatedAt

	// A new version replaces the old, whether or not parts have merged, and keeps when the feed began.
	time.Sleep(5 * time.Millisecond)
	edited := catSpec("Cats and kittens")
	edited.MinProb = 0.8
	if err := store.SaveFeed(ctx, StoredFeed{Owner: bob, Rkey: "cats", Spec: edited, CreatedAt: created}); err != nil {
		t.Fatal(err)
	}
	if got, _ = store.OwnerFeeds(ctx, bob); len(got) != 1 || got[0].Spec.DisplayName != "Cats and kittens" || got[0].Spec.MinProb != 0.8 {
		t.Fatalf("the newest version wins: %+v", got)
	}
	if !got[0].CreatedAt.Equal(created) || !got[0].UpdatedAt.After(created) {
		t.Errorf("created %v, updated %v: the feed keeps its birthday through edits", got[0].CreatedAt, got[0].UpdatedAt)
	}

	// People's feeds are their own: the same rkey under another account is another feed.
	time.Sleep(5 * time.Millisecond)
	if err := store.SaveFeed(ctx, StoredFeed{Owner: alice, Rkey: "cats", Spec: catSpec("Alice's cats")}); err != nil {
		t.Fatal(err)
	}
	all, _ := store.ListFeeds(ctx)
	if rkeys(all) != bob+"/cats "+alice+"/cats" {
		t.Errorf("oldest first, one per account: %s", rkeys(all))
	}
	if a, _ := store.OwnerFeeds(ctx, alice); len(a) != 1 || a[0].Spec.DisplayName != "Alice's cats" {
		t.Errorf("alice: %+v", a)
	}
	if n, _ := store.OwnerFeeds(ctx, "did:plc:nobody"); len(n) != 0 {
		t.Errorf("an account with no feeds: %+v", n)
	}

	// Deleting hides a feed (and only that one) and doesn't mind a feed that isn't there.
	time.Sleep(5 * time.Millisecond)
	if err := store.DeleteFeed(ctx, bob, "cats"); err != nil {
		t.Fatal(err)
	}
	if err := store.DeleteFeed(ctx, bob, "never-was"); err != nil {
		t.Errorf("deleting what isn't there: %v", err)
	}
	if err := store.DeleteFeed(ctx, bob, "cats"); err != nil {
		t.Errorf("deleting twice: %v", err)
	}
	all, _ = store.ListFeeds(ctx)
	if rkeys(all) != alice+"/cats" {
		t.Errorf("only alice's is left: %s", rkeys(all))
	}
	if b, _ := store.OwnerFeeds(ctx, bob); len(b) != 0 {
		t.Errorf("bob's feed is gone: %+v", b)
	}

	// Saving it again brings it back, as a new feed.
	time.Sleep(5 * time.Millisecond)
	if err := store.SaveFeed(ctx, StoredFeed{Owner: bob, Rkey: "cats", Spec: catSpec("Cats again")}); err != nil {
		t.Fatal(err)
	}
	if b, _ := store.OwnerFeeds(ctx, bob); len(b) != 1 || b[0].Spec.DisplayName != "Cats again" {
		t.Errorf("brought back: %+v", b)
	}
}

func TestSeedingTheFeedStoreFromTheConfigFile(t *testing.T) {
	store, _ := scratchFeedStore(t)
	ctx := context.Background()
	cfg, paths := realConfig(t)
	const owner = "did:plc:owner"

	var want []string
	for _, f := range cfg.Feeds {
		if f.Personal == nil {
			want = append(want, f.Rkey)
		}
	}
	n, err := store.SeedFeeds(ctx, owner, cfg.Feeds)
	if err != nil || n != len(want) {
		t.Fatalf("seeded %d of %d: %v", n, len(want), err)
	}
	stored, err := store.OwnerFeeds(ctx, owner)
	if err != nil {
		t.Fatal(err)
	}
	var order []string
	for _, s := range stored {
		order = append(order, s.Rkey)
	}
	if strings.Join(order, " ") != strings.Join(want, " ") {
		t.Errorf("the file's order is kept:\n  got  %v\n  want %v", order, want)
	}
	// What is served from the database is what was served from the file.
	byKey := map[string]Feed{}
	for _, f := range cfg.Feeds {
		byKey[f.Rkey] = f
	}
	for _, s := range stored {
		got := s.Feed(owner)
		if !reflect.DeepEqual(got, byKey[s.Rkey]) {
			t.Errorf("%s differs from the file's:\n  got  %+v\n  want %+v", s.Rkey, got, byKey[s.Rkey])
		}
		if err := got.validate(paths); err != nil {
			t.Errorf("%s: %v", s.Rkey, err)
		}
	}

	// Seeding again changes nothing, even after the feed has been edited on the web.
	edited := stored[0]
	edited.Spec.MinProb = 0.99
	if err := store.SaveFeed(ctx, edited); err != nil {
		t.Fatal(err)
	}
	if n, err := store.SeedFeeds(ctx, owner, cfg.Feeds); err != nil || n != 0 {
		t.Errorf("seeded again: %d, %v", n, err)
	}
	if again, _ := store.OwnerFeeds(ctx, owner); again[0].Spec.MinProb != 0.99 || len(again) != len(want) {
		t.Errorf("an edit is not undone by a restart: %+v", again[0].Spec)
	}

	// A feed taken down on the web stays down.
	time.Sleep(5 * time.Millisecond)
	if err := store.DeleteFeed(ctx, owner, stored[1].Rkey); err != nil {
		t.Fatal(err)
	}
	if n, err := store.SeedFeeds(ctx, owner, cfg.Feeds); err != nil || n != 0 {
		t.Errorf("seeded after a delete: %d, %v", n, err)
	}
	if after, _ := store.OwnerFeeds(ctx, owner); len(after) != len(want)-1 {
		t.Errorf("a deleted feed came back: %d feeds", len(after))
	}

	// Someone else's feeds don't count: the same file seeds another account too.
	if n, err := store.SeedFeeds(ctx, "did:plc:other", cfg.Feeds[:1]); err != nil || n != boolInt(cfg.Feeds[0].Personal == nil) {
		t.Errorf("another account: %d, %v", n, err)
	}
}

func boolInt(b bool) int {
	if b {
		return 1
	}
	return 0
}
