package feedgen

import (
	"context"
	"fmt"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/haileyok/topic-feed/internal/chdb"
	"github.com/haileyok/topic-feed/internal/labelpolicy"
)

type factsPostRow struct {
	URI          string    `ch:"uri"`
	DID          string    `ch:"did"`
	RKey         string    `ch:"rkey"`
	CID          string    `ch:"cid"`
	CreatedAt    time.Time `ch:"created_at"`
	IndexedAt    time.Time `ch:"indexed_at"`
	Text         string    `ch:"text"`
	Langs        []string  `ch:"langs"`
	DetectedLang string    `ch:"detected_lang"`
	EmbedType    string    `ch:"embed_type"`
	LinkDomain   string    `ch:"link_domain"`
	SelfLabels   []string  `ch:"self_labels"`
	MediaKinds   []string  `ch:"media_kinds"`
	MediaCIDs    []string  `ch:"media_cids"`
}

type factsTextRow struct {
	URI       string    `ch:"uri"`
	Text      string    `ch:"text"`
	IndexedAt time.Time `ch:"indexed_at"`
}

type factsRetryRow struct {
	URI           string    `ch:"uri"`
	DID           string    `ch:"did"`
	IndexedAt     time.Time `ch:"indexed_at"`
	Status        string    `ch:"status"`
	Attempts      uint8     `ch:"attempts"`
	NextAttemptAt time.Time `ch:"next_attempt_at"`
	LastError     string    `ch:"last_error"`
	QueuedAt      time.Time `ch:"queued_at"`
	UpdatedAt     time.Time `ch:"updated_at"`
}

type factsLikeHour struct {
	URI   string    `ch:"subject_uri"`
	Hour  time.Time `ch:"hour"`
	Likes uint64    `ch:"likes"`
}

type factsEngagementHour struct {
	URI  string    `ch:"subject_uri"`
	Kind string    `ch:"kind"`
	Hour time.Time `ch:"hour"`
	N    uint64    `ch:"n"`
}

// TestPostFactsAgainstClickHouse runs the inspector's reads on a real server, from the schema files the
// service is deployed with: every column it names exists, a post's pieces come back together, and a post
// we only saw (not stored) is told from one we never did. It runs only when TOPICFEED_CLICKHOUSE_TEST is
// set, in a database of its own.
func TestPostFactsAgainstClickHouse(t *testing.T) {
	if os.Getenv("TOPICFEED_CLICKHOUSE_TEST") == "" {
		t.Skip("set TOPICFEED_CLICKHOUSE_TEST=1 (and CLICKHOUSE_PASSWORD) to run against a ClickHouse server")
	}
	policy, err := labelpolicyForTest()
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	admin, err := chdb.Open(ctx, chdb.FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	scratch := fmt.Sprintf("scratch_facts_%d", time.Now().UnixNano())
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
	// The tables and columns the reads use, made from the schema files.
	for _, c := range []struct {
		file   string
		tables []string
		alters []string
	}{
		{"001_init.sql", []string{"posts", "post_texts", "like_counts_hourly", "deletions", "account_status"}, []string{"ALTER TABLE posts"}},
		{"002_media_labels.sql", nil, []string{"ALTER TABLE posts"}},
		{"003_mod_labels.sql", []string{"mod_labels"}, nil},
		{"004_post_pipeline.sql", []string{"post_pipeline"}, nil},
		{"005_predictions.sql", nil, []string{"ALTER TABLE post_pipeline"}},
		{"006_engagement.sql", []string{"engagement_hourly"}, nil},
		{"008_image_retry.sql", []string{"image_retry_queue"}, nil},
		{"013_post_pipeline_pictures.sql", nil, []string{"ALTER TABLE post_pipeline"}},
	} {
		for _, s := range schemaStatements(t, c.file) {
			run := false
			for _, table := range c.tables {
				run = run || strings.HasPrefix(s, "CREATE TABLE IF NOT EXISTS "+table)
			}
			for _, a := range c.alters {
				run = run || strings.HasPrefix(s, a)
			}
			if run {
				exec(s)
			}
		}
	}

	at := time.Now().UTC().Truncate(time.Second)
	hour := at.Truncate(time.Hour)
	const (
		did    = "did:plc:author"
		stored = "at://did:plc:author/app.bsky.feed.post/stored1"
		reply  = "at://did:plc:author/app.bsky.feed.post/reply1"
		never  = "at://did:plc:author/app.bsky.feed.post/never1"
	)
	if err := chdb.Insert(ctx, conn, "posts", []factsPostRow{{URI: stored, DID: did, RKey: "stored1", CID: "bafy", CreatedAt: at, IndexedAt: at.Add(-time.Hour),
		Text: "models, models, models", Langs: []string{"en"}, DetectedLang: "en", EmbedType: "images", LinkDomain: "", SelfLabels: []string{"graphic-media"},
		MediaKinds: []string{"image"}, MediaCIDs: []string{"bafyimg"}}}); err != nil {
		t.Fatal(err)
	}
	if err := chdb.Insert(ctx, conn, "post_pipeline", []diffPipelineRow{{URI: stored, DID: did, IndexedAt: at.Add(-time.Hour), ProcessedAt: at.Add(-59 * time.Minute),
		FeedPolicy: labelpolicy.OK, Labels: []string{"graphic-media"}, Model: "v5", BroadProbs: map[string]float32{"technology": 0.9}, PathProbs: map[string]float32{"technology/ai": 0.8},
		Signals: map[string]float32{"substance": 0.5}, Tone: map[string]float32{"informative": 0.6}, TopBroad: "technology", TopPath: "technology/ai", TopPathP: 0.8}}); err != nil {
		t.Fatal(err)
	}
	if err := chdb.Insert(ctx, conn, "post_texts", []factsTextRow{{URI: stored, Text: "models", IndexedAt: at}, {URI: reply, Text: "a reply nobody keeps", IndexedAt: at}}); err != nil {
		t.Fatal(err)
	}
	if err := chdb.Insert(ctx, conn, "like_counts_hourly", []factsLikeHour{{stored, hour, 5}, {stored, hour.Add(-time.Hour), 2}}); err != nil {
		t.Fatal(err)
	}
	if err := chdb.Insert(ctx, conn, "engagement_hourly", []factsEngagementHour{{stored, "repost", hour, 3}, {stored, "reply", hour, 4}, {stored, "quote", hour, 1}}); err != nil {
		t.Fatal(err)
	}
	if err := chdb.Insert(ctx, conn, "image_retry_queue", []factsRetryRow{{URI: stored, DID: did, IndexedAt: at.Add(-time.Hour), Status: "pending", Attempts: 2,
		NextAttemptAt: at, LastError: "1 of 1 pictures did not download", QueuedAt: at, UpdatedAt: at}}); err != nil {
		t.Fatal(err)
	}
	if err := chdb.Insert(ctx, conn, "account_status", []diffAccount{{DID: did, Active: 0, Status: "suspended", IndexedAt: at}}); err != nil {
		t.Fatal(err)
	}
	if err := chdb.Insert(ctx, conn, "deletions", []diffDeletion{{DID: did, Collection: "app.bsky.feed.post", RKey: "stored1", URI: stored, IndexedAt: at}}); err != nil {
		t.Fatal(err)
	}

	store := &Store{Conn: conn, Policy: policy}

	got, err := store.PostFacts(ctx, stored, did)
	if err != nil {
		t.Fatal(err)
	}
	if got.Stored == nil || got.Stored.Text != "models, models, models" || got.Stored.CID != "bafy" || got.Stored.EmbedType != "images" ||
		len(got.Stored.Langs) != 1 || got.Stored.Langs[0] != "en" || got.Stored.MediaKinds[0] != "image" || got.Stored.SelfLabels[0] != "graphic-media" {
		t.Errorf("stored post: %+v", got.Stored)
	}
	if got.Pipeline == nil || got.Pipeline.Model != "v5" || got.Pipeline.FeedPolicy != labelpolicy.OK || got.Pipeline.PathProbs["technology/ai"] != 0.8 ||
		got.Pipeline.BroadProbs["technology"] != 0.9 || got.Pipeline.Tone["informative"] != 0.6 || got.Pipeline.Signals["substance"] != 0.5 ||
		got.Pipeline.TopPath != "technology/ai" || got.Pipeline.TopPathP != 0.8 || got.Pipeline.Labels[0] != "graphic-media" {
		t.Errorf("pipeline: %+v", got.Pipeline)
	}
	if got.Post.Likes != 7 || got.Post.Reposts != 3 || got.Post.Replies != 4 || got.Post.Quotes != 1 {
		t.Errorf("engagement: %+v", got.Post)
	}
	if got.Post.TopPath != "technology/ai" || got.Post.Signals["substance"] != 0.5 {
		t.Errorf("the pipeline's scores are on the post the feeds judge: %+v", got.Post)
	}
	if !got.Deleted || !got.AuthorInactive || !got.SeenText {
		t.Errorf("deleted %v, inactive %v, text seen %v", got.Deleted, got.AuthorInactive, got.SeenText)
	}
	if got.RetryStatus != "pending" || got.RetryAttempts != 2 || !strings.Contains(got.RetryError, "did not download") {
		t.Errorf("retry: %q %d %q", got.RetryStatus, got.RetryAttempts, got.RetryError)
	}
	in := got.EvalInput(policy)
	if in.Model != "v5" || in.PathProbs["technology/ai"] != 0.8 || !in.Deleted || !in.AuthorInactive || in.Policy != policy {
		t.Errorf("what the feeds judge: %+v", in)
	}

	// A post we saw and didn't store (a reply): its text is there, nothing else.
	rf, err := store.PostFacts(ctx, reply, did)
	if err != nil {
		t.Fatal(err)
	}
	if rf.Stored != nil || rf.Pipeline != nil || !rf.SeenText || rf.Deleted || rf.RetryStatus != "" {
		t.Errorf("a reply: %+v", rf)
	}
	// A post we never saw: nothing at all.
	nf, err := store.PostFacts(ctx, never, "did:plc:nobody")
	if err != nil {
		t.Fatal(err)
	}
	if nf.Stored != nil || nf.Pipeline != nil || nf.SeenText || nf.AuthorInactive || nf.Post.Likes != 0 {
		t.Errorf("a post never seen: %+v", nf)
	}
}

func labelpolicyForTest() (*labelpolicy.Policy, error) {
	return labelpolicy.Load("../../config/label_policy.yaml")
}
