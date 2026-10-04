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
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	atmosidentity "github.com/jcalabro/atmos/identity"
	"gopkg.in/yaml.v3"

	"github.com/haileyok/topic-feed/internal/taxonomy"
)

var now = time.Date(2026, 9, 29, 12, 0, 0, 0, time.UTC)

func posts(n int) []Post {
	out := make([]Post, n)
	for i := range out {
		out[i] = Post{URI: fmt.Sprintf("at://did:plc:a%d/app.bsky.feed.post/%03d", i, 999-i),
			DID: fmt.Sprintf("did:plc:a%d", i), IndexedAt: now.Add(-time.Duration(i) * time.Minute),
			TopPath: "sports/american_football", TopPathP: 0.91234,
			TopPaths: []string{"sports/american_football", "sports/other", "sports/soccer"}, TopPs: []float32{0.91234, 0.05, 0.01},
			Tone: map[string]float32{"informative": 0.2, "outraged": 0.7, "humorous": 0.1}}
	}
	return out
}

func uris(ps []Post) []string {
	out := make([]string, len(ps))
	for i, p := range ps {
		out[i] = p.URI
	}
	return out
}

func TestScoreEngagementAndDecay(t *testing.T) {
	r := DefaultRanking
	fresh := Post{URI: "fresh", DID: "a", IndexedAt: now}
	liked := Post{URI: "liked", DID: "b", IndexedAt: now.Add(-time.Hour), Likes: 50}
	old := Post{URI: "old", DID: "c", IndexedAt: now.Add(-20 * time.Hour), Likes: 50}
	ps := []Post{fresh, liked, old}
	Score(ps, Feed{Ranking: r}, now)
	if !(ps[1].Score > ps[0].Score) {
		t.Errorf("an hour-old post with 50 likes should beat a fresh one: %v vs %v", ps[1].Score, ps[0].Score)
	}
	if !(ps[0].Score > ps[2].Score) {
		t.Errorf("a fresh post should beat a 20-hour-old one with 50 likes: %v vs %v", ps[0].Score, ps[2].Score)
	}
	promo := []Post{{URI: "p", IndexedAt: now, Signals: map[string]float32{"promo": 1}},
		{URI: "q", IndexedAt: now, Signals: map[string]float32{"substance": 1}}}
	Score(promo, Feed{Ranking: r}, now)
	if !(promo[1].Score > promo[0].Score) {
		t.Error("with no engagement, a substantive post should beat a promotional one")
	}
}

func TestRankFreshSlotsAndEveryPostOnce(t *testing.T) {
	ps := posts(20)
	// the oldest posts have the most likes, so score order is oldest first
	for i := range ps {
		ps[i].Likes = uint64(i * 100)
	}
	r := DefaultRanking
	r.AuthorGap = 0
	out := Rank(ps, Feed{Ranking: r}, now)
	if len(out) != 20 {
		t.Fatalf("%d posts", len(out))
	}
	seen := map[string]bool{}
	for _, p := range out {
		if seen[p.URI] {
			t.Fatalf("%s twice", p.URI)
		}
		seen[p.URI] = true
	}
	if out[0].URI != ps[19].URI {
		t.Errorf("slot 1 should be the highest score, got %s", out[0].URI)
	}
	if out[3].URI != ps[0].URI || out[7].URI != ps[1].URI {
		t.Errorf("slots 4 and 8 should be the two newest posts, got %s, %s", out[3].URI, out[7].URI)
	}
}

func TestRankAuthorGap(t *testing.T) {
	ps := posts(12)
	for i := 0; i < 3; i++ { // one author has the three best posts
		ps[i].DID = "did:plc:loud"
		ps[i].Likes = 1000
	}
	r := DefaultRanking
	r.FreshEvery = 0
	r.AuthorGap = 5
	out := Rank(ps, Feed{Ranking: r}, now)
	var slots []int
	for i, p := range out {
		if p.DID == "did:plc:loud" {
			slots = append(slots, i)
		}
	}
	if len(slots) != 3 || slots[0] != 0 || slots[1] < 5 || slots[2]-slots[1] < 5 {
		t.Errorf("author slots %v, want at least 5 apart", slots)
	}
}

type swapBuilder struct{ n int }

// Each build reverses the order the previous one had, like engagement shifting.
func (b *swapBuilder) Build(context.Context, Feed, time.Time, int) ([]Post, Removed, error) {
	b.n++
	ps := posts(10)
	for i := range ps {
		if b.n%2 == 0 {
			ps[i].Likes = uint64(i * 1000)
		} else {
			ps[i].Likes = uint64((10 - i) * 1000)
		}
	}
	return ps, Removed{}, nil
}

func TestPagingStaysOnOneBuild(t *testing.T) {
	cfg := &Config{Feeds: []Feed{{Rkey: "f", Ranking: Ranking{Gravity: 1.8}}}}
	fs := NewFeeds(cfg, &swapBuilder{}, slog.New(slog.NewTextHandler(io.Discard, nil)), 48*time.Hour, time.Hour, 100)
	fs.buildNow(context.Background(), cfg.Feeds[0])
	firstItems, next, _, _ := fs.Page(context.Background(), "f", "", 4)
	fs.buildNow(context.Background(), cfg.Feeds[0]) // the order changes
	var all []string
	for _, it := range firstItems {
		all = append(all, it.URI)
	}
	for next != "" {
		var p []Item
		p, next, _, _ = fs.Page(context.Background(), "f", next, 4)
		for _, it := range p {
			all = append(all, it.URI)
		}
	}
	seen := map[string]bool{}
	for _, u := range all {
		if seen[u] {
			t.Fatalf("%s repeated across pages", u)
		}
		seen[u] = true
	}
	if len(all) != 10 {
		t.Errorf("paged %d posts, want 10", len(all))
	}
	// A cursor from an expired build continues at the same position in the current one.
	p, _, _, err := fs.Page(context.Background(), "f", "12345:8", 4)
	if err != nil || len(p) != 2 {
		t.Errorf("expired cursor: %v %v", p, err)
	}
	for _, c := range []string{"x", "12:", ":3", "-1:2", "5:-1"} {
		if _, _, _, err := fs.Page(context.Background(), "f", c, 4); err == nil {
			t.Errorf("cursor %q accepted", c)
		}
	}
}

func TestRankingDefaultsAndOverrides(t *testing.T) {
	var c Config
	err := yaml.Unmarshal([]byte(`feeds:
  - rkey: a
    display_name: A
    paths: [technology/ai]
    min_prob: 0.5
  - rkey: b
    display_name: B
    paths: [technology/ai]
    min_prob: 0.5
    ranking: {gravity: 1.2, fresh_every: 0, weights: {like: 3}}
`), &c)
	if err != nil {
		t.Fatal(err)
	}
	if c.Feeds[0].Ranking != DefaultRanking {
		t.Errorf("feed a: %+v", c.Feeds[0].Ranking)
	}
	b := c.Feeds[1].Ranking
	if b.Gravity != 1.2 || b.FreshEvery != 0 || b.Weights.Like != 3 || b.Weights.Quote != DefaultRanking.Weights.Quote || b.AuthorGap != DefaultRanking.AuthorGap {
		t.Errorf("feed b: %+v", b)
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
	good := Feed{Rkey: "ok", DisplayName: "x", Paths: []string{"world_news", "technology/ai"}, MinProb: 0.5,
		Exclude: map[string]float32{"adult_content": 0.2, "art/commissions": 0.3}}
	if err := (&Config{Feeds: []Feed{good}}).Validate(paths); err != nil {
		t.Errorf("broad topics and exclusions: %v", err)
	}
	bad := []Feed{
		{Rkey: "Has Caps", DisplayName: "x", Paths: []string{"technology/ai"}, MinProb: 0.5},
		{Rkey: "ok", DisplayName: "x", Paths: []string{"technology/nope"}, MinProb: 0.5},
		{Rkey: "ok", DisplayName: "x", Paths: []string{"nope"}, MinProb: 0.5},
		{Rkey: "ok", DisplayName: "x", Paths: []string{"technology/ai"}, MinProb: 0.5, Exclude: map[string]float32{"adult": 0.2}},
		{Rkey: "ok", DisplayName: "x", Paths: []string{"technology/ai"}, MinProb: 0.5, Exclude: map[string]float32{"adult_content": 2}},
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

func testServer(t *testing.T) *Server { return testServerWith(t, testDirectory()) }

func testServerWith(t *testing.T, dir *atmosidentity.Directory) *Server {
	cfg := &Config{Feeds: []Feed{{Rkey: "nfl", DisplayName: "NFL", Paths: []string{"sports/american_football"}, MinProb: 0.5, Ranking: DefaultRanking}}}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fs := NewFeeds(cfg, fakeBuilder{posts(5)}, log, 48*time.Hour, time.Hour, 100)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	fs.Start(ctx)
	return NewServer(ServerConfig{Hostname: "feeds.example.com", ServiceDID: "did:web:feeds.example.com",
		OwnerDID: syntax.DID("did:plc:owner"), MaxAge: time.Minute}, fs, dir, log)
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
	if code != 200 || len(body["feed"].([]any)) != 2 || body["cursor"] == nil || !strings.HasPrefix(body["reqId"].(string), "nfl-") {
		t.Fatalf("page 1: %d %v", code, body)
	}
	item := body["feed"].([]any)[0].(map[string]any)
	var fc struct {
		ID    string  `json:"id"`
		Topic string  `json:"topic"`
		P     float64 `json:"p"`
		Top   [][]any `json:"top"`
		Tone  [][]any `json:"tone"`
	}
	if err := json.Unmarshal([]byte(item["feedContext"].(string)), &fc); err != nil || fc.ID != item["post"] ||
		fc.Topic != "sports/american_football" || fc.P != 0.912 || len(fc.Top) != 3 ||
		fc.Top[1][0] != "sports/other" || fc.Top[1][1] != 0.05 || len(fc.Tone) != 3 || fc.Tone[0][0] != "outraged" || fc.Tone[0][1] != 0.7 {
		t.Errorf("feedContext %v (%v)", item["feedContext"], err)
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

type fakeSink struct{ rows []InteractionRow }

func (f *fakeSink) Add(r []InteractionRow) int { f.rows = append(f.rows, r...); return 0 }

func TestSendInteractions(t *testing.T) {
	viewer := newTestViewer(t, "did:plc:viewer")
	s := testServerWith(t, testDirectory(viewer.doc()))
	sink := &fakeSink{}
	s.Interactions = sink

	post := func(aud string, method syntax.NSID, body string) int {
		req := httptest.NewRequest(http.MethodPost, "/xrpc/app.bsky.feed.sendInteractions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		if aud != "" {
			req.Header.Set("Authorization", "Bearer "+viewer.credential(t, aud, method))
		}
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		return rec.Code
	}
	withFeed := `{"feed":"at://did:plc:owner/app.bsky.feed.generator/nfl","interactions":[
		{"item":"at://did:plc:a/app.bsky.feed.post/1","event":"app.bsky.feed.defs#interactionSeen","feedContext":"{\"id\":\"at://did:plc:a/app.bsky.feed.post/1\"}","reqId":"nfl-abc"}]}`
	if code := post("did:web:feeds.example.com", interactionsMethod, withFeed); code != 200 {
		t.Fatalf("valid credential: %d", code)
	}
	// Bluesky may address the service with its #bsky_fg fragment; older clients omit feed.
	noFeed := `{"interactions":[{"item":"at://did:plc:a/app.bsky.feed.post/2","event":"app.bsky.feed.defs#requestLess","reqId":"nfl-def"}]}`
	if code := post("did:web:feeds.example.com#bsky_fg", interactionsMethod, noFeed); code != 200 {
		t.Fatalf("fragment audience: %d", code)
	}
	if len(sink.rows) != 2 {
		t.Fatalf("rows %+v", sink.rows)
	}
	r := sink.rows[0]
	if r.ViewerDID != "did:plc:viewer" || r.Feed != "nfl" || r.Event != "interactionSeen" ||
		r.Item != "at://did:plc:a/app.bsky.feed.post/1" || r.ReqID != "nfl-abc" || !strings.Contains(r.FeedContext, "post/1") {
		t.Errorf("row %+v", r)
	}
	if sink.rows[1].Feed != "nfl" || sink.rows[1].Event != "requestLess" {
		t.Errorf("feed from reqId: %+v", sink.rows[1])
	}
	// No credential, a credential for another method, or for another service: rejected.
	for name, code := range map[string]int{
		"none":          post("", interactionsMethod, withFeed),
		"wrong method":  post("did:web:feeds.example.com", skeletonMethod, withFeed),
		"wrong service": post("did:web:elsewhere.example.com", interactionsMethod, withFeed),
	} {
		if code != 401 {
			t.Errorf("%s: %d, want 401", name, code)
		}
	}
	if code := post("did:web:feeds.example.com", interactionsMethod, "not json"); code != 400 {
		t.Errorf("bad body: %d", code)
	}
	if len(sink.rows) != 2 {
		t.Errorf("rejected calls stored rows: %d", len(sink.rows))
	}
}

func TestAcceptsInteractionsDefault(t *testing.T) {
	var c Config
	if err := yaml.Unmarshal([]byte("feeds:\n  - rkey: a\n  - rkey: b\n    accepts_interactions: false\n"), &c); err != nil {
		t.Fatal(err)
	}
	if !c.Feeds[0].AcceptsInteractions || c.Feeds[1].AcceptsInteractions {
		t.Errorf("%v %v", c.Feeds[0].AcceptsInteractions, c.Feeds[1].AcceptsInteractions)
	}
}

func TestToneRules(t *testing.T) {
	angry := map[string]float32{"outraged": 0.9, "informative": 0.1}
	calm := map[string]float32{"informative": 0.8, "supportive": 0.2}
	cut := Rules{Max: map[string]float32{"outraged": 0.5}}
	if cut.Allows(angry) || !cut.Allows(calm) {
		t.Error("max cutoff")
	}
	funny := Rules{Min: map[string]float32{"humorous": 0.5}}
	if funny.Allows(calm) || !funny.Allows(map[string]float32{"humorous": 0.6}) {
		t.Error("min cutoff")
	}
	nudge := Rules{Weights: map[string]float64{"outraged": -2, "supportive": 1}}
	ps := []Post{{URI: "a", IndexedAt: now, Tone: angry}, {URI: "b", IndexedAt: now, Tone: calm}}
	Score(ps, Feed{Ranking: DefaultRanking, Tone: nudge}, now)
	if !(ps[1].Score > ps[0].Score) {
		t.Errorf("nudge: calm %v should beat angry %v", ps[1].Score, ps[0].Score)
	}
	paths := map[string]bool{"technology/ai": true}
	for _, bad := range []Rules{
		{Max: map[string]float32{"angry": 0.5}},
		{Min: map[string]float32{"humorous": 1.5}},
		{Weights: map[string]float64{"sad": 1}},
	} {
		f := Feed{Rkey: "x", DisplayName: "X", Paths: []string{"technology/ai"}, MinProb: 0.5, Ranking: DefaultRanking, Tone: bad}
		if err := (&Config{Feeds: []Feed{f}}).Validate(paths); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
}

func TestSplitPaths(t *testing.T) {
	subs, broads := split([]string{"world_news", "technology/ai", "art"})
	if !slices.Equal(subs, []string{"technology/ai"}) || !slices.Equal(broads, []string{"world_news", "art"}) {
		t.Errorf("%v %v", subs, broads)
	}
	subs, broads = split([]string{"technology/ai"})
	if !slices.Equal(broads, []string{""}) || len(subs) != 1 {
		t.Errorf("empty side must be [\"\"]: %v %v", subs, broads)
	}
}

func TestSignalRulesAndAnyTopic(t *testing.T) {
	paths := map[string]bool{"technology/ai": true}
	ok := Feed{Rkey: "deep", DisplayName: "Deep", Paths: []string{AnyTopic}, MinProb: 0.5, Ranking: DefaultRanking,
		Signals: Rules{Min: map[string]float32{"substance": 0.8}, Weights: map[string]float64{"news": 1}}}
	if err := (&Config{Feeds: []Feed{ok}}).Validate(paths); err != nil {
		t.Errorf("any topic with signal rules: %v", err)
	}
	for _, bad := range []Feed{
		{Rkey: "x", DisplayName: "X", Paths: []string{AnyTopic, "technology/ai"}, MinProb: 0.5},
		{Rkey: "x", DisplayName: "X", Paths: []string{AnyTopic}, MinProb: 0.5, Signals: Rules{Min: map[string]float32{"vibes": 0.5}}},
		{Rkey: "x", DisplayName: "X", Paths: []string{AnyTopic}, MinProb: 0.5, Tone: Rules{Weights: map[string]float64{"humorous": 50}}},
	} {
		if err := (&Config{Feeds: []Feed{bad}}).Validate(paths); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	news := []Post{{URI: "a", IndexedAt: now}, {URI: "b", IndexedAt: now, Signals: map[string]float32{"news": 1}}}
	Score(news, Feed{Ranking: DefaultRanking, Signals: Rules{Weights: map[string]float64{"news": 2}}}, now)
	if !(news[1].Score > news[0].Score) {
		t.Error("signal nudge")
	}
}

func TestHomepageLinkPreview(t *testing.T) {
	s := testServer(t)
	req := httptest.NewRequest(http.MethodGet, "/", nil)
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	body := rec.Body.String()
	for _, want := range []string{
		`<meta property="og:image" content="https://feeds.example.com/static/og.png">`,
		`<meta property="og:url" content="https://feeds.example.com/">`,
		`<meta name="twitter:card" content="summary_large_image">`,
	} {
		if !strings.Contains(body, want) {
			t.Errorf("missing %s", want)
		}
	}
	if strings.Contains(body, "__ORIGIN__") {
		t.Error("placeholder left in the page")
	}
	img := httptest.NewRecorder()
	s.Handler().ServeHTTP(img, httptest.NewRequest(http.MethodGet, "/static/og.png", nil))
	if img.Code != 200 || img.Header().Get("Content-Type") != "image/png" {
		t.Errorf("og.png: %d %s", img.Code, img.Header().Get("Content-Type"))
	}
}
