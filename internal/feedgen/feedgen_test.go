package feedgen

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/topic-feed/internal/taxonomy"
)

func posts(n int) []Post {
	base := time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)
	out := make([]Post, n)
	for i := range out {
		// newest first; every pair of posts shares a timestamp to exercise the URI tiebreak
		out[i] = Post{URI: fmt.Sprintf("at://did:plc:a/app.bsky.feed.post/%03d", 999-i), IndexedAt: base.Add(-time.Duration(i/2) * time.Second)}
	}
	return out
}

func TestPageWalksWholeFeed(t *testing.T) {
	all := posts(25)
	var got []Post
	cursor := ""
	for range 10 {
		p, next, err := page(all, cursor, 7)
		if err != nil {
			t.Fatal(err)
		}
		got = append(got, p...)
		if next == "" {
			break
		}
		cursor = next
	}
	if len(got) != len(all) {
		t.Fatalf("got %d posts, want %d", len(got), len(all))
	}
	for i := range all {
		if got[i].URI != all[i].URI {
			t.Fatalf("post %d: %s, want %s", i, got[i].URI, all[i].URI)
		}
	}
}

func TestPageCursorSurvivesNewPosts(t *testing.T) {
	all := posts(10)
	_, next, _ := page(all, "", 4)
	// new posts arrive at the top; the next page still starts after the 4th original post
	newer := append([]Post{{URI: "at://did:plc:a/app.bsky.feed.post/zzz", IndexedAt: all[0].IndexedAt.Add(time.Minute)}}, all...)
	p, _, _ := page(newer, next, 4)
	if p[0].URI != all[4].URI {
		t.Errorf("first post of page 2 is %s, want %s", p[0].URI, all[4].URI)
	}
}

func TestBadCursor(t *testing.T) {
	for _, c := range []string{"x", "12::nope", "abc::at://x", "::at://x"} {
		if _, _, err := page(posts(3), c, 2); err == nil {
			t.Errorf("cursor %q accepted", c)
		}
	}
}

func TestRepoConfig(t *testing.T) {
	tax, err := taxonomy.Load("../../taxonomy/v1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := LoadConfig("../../config/feeds.yaml", tax); err != nil {
		t.Fatal(err)
	}
	paths := TaxonomyPaths(tax)
	bad := []Feed{
		{Rkey: "Has Caps", DisplayName: "x", Paths: []string{"technology/ai"}, MinProb: 0.5},
		{Rkey: "ok", DisplayName: "x", Paths: []string{"technology/nope"}, MinProb: 0.5},
		{Rkey: "ok", DisplayName: "x", Paths: []string{"technology/ai"}, MinProb: 0},
		{Rkey: "ok", DisplayName: "", Paths: []string{"technology/ai"}, MinProb: 0.5},
	}
	for _, f := range bad {
		if err := (&Config{Feeds: []Feed{f}}).Validate(paths); err == nil {
			t.Errorf("accepted %+v", f)
		}
	}
}

type fakeBuilder struct{ posts []Post }

func (b fakeBuilder) Build(context.Context, Feed, time.Time, int) ([]Post, Removed, error) {
	return b.posts, Removed{}, nil
}

func testServer(t *testing.T) *Server {
	cfg := &Config{Feeds: []Feed{{Rkey: "nfl", DisplayName: "NFL", Paths: []string{"sports/american_football"}, MinProb: 0.5}}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fs := NewFeeds(cfg, fakeBuilder{posts(5)}, log, 48*time.Hour, time.Hour, 100)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	fs.Start(ctx)
	return NewServer(ServerConfig{Hostname: "feeds.example.com", ServiceDID: "did:web:feeds.example.com",
		OwnerDID: syntax.DID("did:plc:owner"), MaxAge: time.Minute}, fs, identity.NewMockDirectory(), log)
}

func get(t *testing.T, s *Server, path string, hdr ...string) (int, map[string]any) {
	req := httptest.NewRequest(http.MethodGet, path, nil)
	if len(hdr) == 2 {
		req.Header.Set(hdr[0], hdr[1])
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	var body map[string]any
	json.Unmarshal(rec.Body.Bytes(), &body)
	return rec.Code, body
}

func TestSkeletonEndpoint(t *testing.T) {
	s := testServer(t)
	feed := url.QueryEscape("at://did:plc:owner/app.bsky.feed.generator/nfl")
	code, body := get(t, s, "/xrpc/app.bsky.feed.getFeedSkeleton?limit=2&feed="+feed)
	if code != 200 || len(body["feed"].([]any)) != 2 || body["cursor"] == nil {
		t.Fatalf("page 1: %d %v", code, body)
	}
	code, body = get(t, s, "/xrpc/app.bsky.feed.getFeedSkeleton?limit=10&feed="+feed+"&cursor="+url.QueryEscape(body["cursor"].(string)))
	if code != 200 || len(body["feed"].([]any)) != 3 || body["cursor"] != nil {
		t.Fatalf("page 2: %d %v", code, body)
	}
	// an invalid credential still gets the feed
	if code, _ := get(t, s, "/xrpc/app.bsky.feed.getFeedSkeleton?feed="+feed, "Authorization", "Bearer not-a-jwt"); code != 200 {
		t.Errorf("invalid credential: %d", code)
	}
	for _, q := range []string{
		"feed=" + url.QueryEscape("at://did:plc:someoneelse/app.bsky.feed.generator/nfl"),
		"feed=" + url.QueryEscape("at://did:plc:owner/app.bsky.feed.generator/nope"),
		"feed=garbage",
	} {
		if code, body := get(t, s, "/xrpc/app.bsky.feed.getFeedSkeleton?"+q); code != 400 || body["error"] != "UnknownFeed" {
			t.Errorf("%s: %d %v", q, code, body)
		}
	}
	if code, _ := get(t, s, "/xrpc/app.bsky.feed.getFeedSkeleton?limit=500&feed="+feed); code != 400 {
		t.Errorf("limit 500: %d", code)
	}
}

func TestDescribeAndDIDDoc(t *testing.T) {
	s := testServer(t)
	_, body := get(t, s, "/xrpc/app.bsky.feed.describeFeedGenerator")
	feeds := body["feeds"].([]any)
	if body["did"] != "did:web:feeds.example.com" || len(feeds) != 1 ||
		feeds[0].(map[string]any)["uri"] != "at://did:plc:owner/app.bsky.feed.generator/nfl" {
		t.Errorf("describe: %v", body)
	}
	_, doc := get(t, s, "/.well-known/did.json")
	svc := doc["service"].([]any)[0].(map[string]any)
	if doc["id"] != "did:web:feeds.example.com" || svc["serviceEndpoint"] != "https://feeds.example.com" || svc["id"] != "#bsky_fg" {
		t.Errorf("did doc: %v", doc)
	}
	if code, body := get(t, s, "/healthz"); code != 200 || body["ok"] != true {
		t.Errorf("healthz: %d %v", code, body)
	}
}
