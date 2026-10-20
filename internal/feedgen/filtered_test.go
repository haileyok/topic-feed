package feedgen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/topic-feed/internal/signin"
)

const testSourceURI = "at://did:plc:source/app.bsky.feed.generator/hot"

func filteredFeed() Feed {
	return Feed{Rkey: "hot-filter", DisplayName: "Hot, filtered", Filtered: &FilteredConfig{Source: testSourceURI},
		Exclude: map[string]float32{"us_politics": 0.5}, Ranking: DefaultRanking, AcceptsInteractions: true}
}

func TestFiltersKeep(t *testing.T) {
	fl := Filters{
		Exclude:    map[string]float32{"us_politics": 0.5, "sports/american_football": 0.4},
		Signals:    Rules{Max: map[string]float32{"critical": 0.6}},
		TopicRules: map[string]TopicRules{"technology": {Signals: Rules{Max: map[string]float32{"critical": 1}}}},
	}
	cases := []struct {
		name string
		p    ScoredPost
		keep bool
	}{
		{"never scored", ScoredPost{}, true},
		{"politics, sure", ScoredPost{Scored: true, BroadProbs: map[string]float32{"us_politics": 0.8}}, false},
		{"politics, at the cutoff", ScoredPost{Scored: true, BroadProbs: map[string]float32{"us_politics": 0.5}}, true},
		{"football subtopic", ScoredPost{Scored: true, PathProbs: map[string]float32{"sports/american_football": 0.45}}, false},
		{"critical", ScoredPost{Scored: true, Signals: map[string]float32{"critical": 0.7}, TopPath: "pets/cats"}, false},
		{"critical about technology", ScoredPost{Scored: true, Signals: map[string]float32{"critical": 0.7}, TopPath: "technology/ai"}, true},
		{"mild", ScoredPost{Scored: true, Signals: map[string]float32{"critical": 0.2}}, true},
	}
	for _, c := range cases {
		if got := fl.Keeps(c.p); got != c.keep {
			t.Errorf("%s: kept %v, want %v", c.name, got, c.keep)
		}
	}
	fl.DropUnscored = true
	if fl.Keeps(ScoredPost{}) {
		t.Error("drop_unscored kept a post the model never scored")
	}

}

func TestFilteredFeedValidation(t *testing.T) {
	paths := map[string]bool{"us_politics": true, "technology": true, "technology/ai": true}
	if err := filteredFeed().validate(paths); err != nil {
		t.Fatalf("a good filtered feed: %v", err)
	}
	bad := map[string]func(*Feed){
		"handle source": func(f *Feed) { f.Filtered.Source = "at://alice.test/app.bsky.feed.generator/x" },
		"not a feed":    func(f *Feed) { f.Filtered.Source = "at://did:plc:x/app.bsky.feed.post/x" },
		"paths":         func(f *Feed) { f.Paths = []string{"technology"} },
		"min_prob":      func(f *Feed) { f.MinProb = 0.5 },
		"weights":       func(f *Feed) { f.Tone = Rules{Weights: map[string]float64{"outraged": -1}} },
		"topic weights": func(f *Feed) {
			f.TopicRules = map[string]TopicRules{"technology": {Tone: Rules{Weights: map[string]float64{"humorous": 1}}}}
		},
		"topic min_prob":  func(f *Feed) { f.TopicRules = map[string]TopicRules{"technology": {MinProb: 0.3}} },
		"unknown topic":   func(f *Feed) { f.Exclude = map[string]float32{"nope": 0.5} },
		"personal too":    func(f *Feed) { f.Personal = &PersonalConfig{} },
		"max age":         func(f *Feed) { f.MaxAgeMinutes = 60 },
		"exclude above 1": func(f *Feed) { f.Exclude = map[string]float32{"us_politics": 2} },
	}
	for name, change := range bad {
		f := filteredFeed()
		f.Filtered = &FilteredConfig{Source: testSourceURI}
		change(&f)
		if err := f.validate(paths); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
}

func TestFeedContextCarriesTheSourcesRequestID(t *testing.T) {
	for _, c := range []struct{ reqID, ctx string }{{"abc", "th-music"}, {"", "x:y:z"}, {"r:1", ""}} {
		id, ctx := unwrapContext(wrapContext(c.reqID, c.ctx))
		if id != c.reqID || ctx != c.ctx {
			t.Errorf("wrap %q %q: back as %q %q", c.reqID, c.ctx, id, ctx)
		}
	}
	if id, ctx := unwrapContext("plain"); id != "" || ctx != "plain" {
		t.Errorf("an unwrapped context: %q %q", id, ctx)
	}
}

// fakeSourceFeed is a source feed: posts p0, p1, ... in pages, with a cursor that is the next index.
type fakeSourceFeed struct {
	mu           sync.Mutex
	total        int
	pages        []url.Values
	auths        []string
	langs        []string
	interactions []map[string]any
	interAuth    []string
	status       int
	got          chan struct{}
}

func (s *fakeSourceFeed) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if r.URL.Path == "/xrpc/app.bsky.feed.sendInteractions" {
		var body map[string]any
		json.NewDecoder(r.Body).Decode(&body)
		s.interactions = append(s.interactions, body)
		s.interAuth = append(s.interAuth, r.Header.Get("Authorization"))
		w.Write([]byte("{}"))
		if s.got != nil {
			s.got <- struct{}{}
		}
		return
	}
	if s.status != 0 {
		w.WriteHeader(s.status)
		w.Write([]byte(`{"error":"AuthRequired"}`))
		return
	}
	q := r.URL.Query()
	s.pages = append(s.pages, q)
	s.auths = append(s.auths, r.Header.Get("Authorization"))
	s.langs = append(s.langs, r.Header.Get("Accept-Language"))
	start, _ := strconv.Atoi(q.Get("cursor"))
	limit, _ := strconv.Atoi(q.Get("limit"))
	type item struct {
		Post        string          `json:"post"`
		Reason      json.RawMessage `json:"reason,omitempty"`
		FeedContext string          `json:"feedContext,omitempty"`
	}
	page := struct {
		Feed   []item `json:"feed"`
		Cursor string `json:"cursor,omitempty"`
		ReqID  string `json:"reqId"`
	}{Feed: []item{}, ReqID: fmt.Sprintf("req-%d", start)}
	end := min(start+limit, s.total)
	for i := start; i < end; i++ {
		it := item{Post: fmt.Sprintf("at://did:plc:a/app.bsky.feed.post/p%d", i), FeedContext: fmt.Sprintf("ctx-%d", i)}
		if i == 1 {
			it.Reason = json.RawMessage(`{"$type":"app.bsky.feed.defs#skeletonReasonRepost","repost":"at://did:plc:b/app.bsky.feed.repost/r"}`)
		}
		page.Feed = append(page.Feed, it)
	}
	if end < s.total {
		page.Cursor = strconv.Itoa(end)
	}
	json.NewEncoder(w).Encode(page)
}

// politicsEveryOther scores every even post as US politics, and leaves every post above 100 unscored.
type politicsEveryOther struct {
	mu      sync.Mutex
	saved   map[string]*Filters
	leftOut []LeftOutRow // what was recorded (it is also the sink)
}

func (s *politicsEveryOther) Add(rows []LeftOutRow) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.leftOut = append(s.leftOut, rows...)
	return 0
}

func (s *politicsEveryOther) LeftOutPosts(_ context.Context, did, rkey string, after LeftOutCursor, limit int) ([]LeftOutPost, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	var out []LeftOutPost
	past := after.URI == "" // rows are newest last: walk back from the end, past the cursor's post
	for i := len(s.leftOut) - 1; i >= 0 && len(out) < limit; i-- {
		r := s.leftOut[i]
		if !past {
			past = r.URI == after.URI
			continue
		}
		if r.ViewerDID == did && r.Feed == rkey {
			out = append(out, LeftOutPost{URI: r.URI, At: r.LeftOutAt, Reason: LeftOutReason{Kind: r.Reason, Name: r.Name, Value: r.Value, Cutoff: r.Cutoff, Bound: r.Bound}})
		}
	}
	return out, nil
}

func (s *politicsEveryOther) ScorePosts(_ context.Context, uris []string) (map[string]ScoredPost, error) {
	out := map[string]ScoredPost{}
	for _, u := range uris {
		n, _ := strconv.Atoi(u[strings.LastIndex(u, "/p")+2:])
		if n > 100 {
			continue
		}
		p := ScoredPost{URI: u, Scored: true, BroadProbs: map[string]float32{"us_politics": 0.1}}
		if n%2 == 0 {
			p.BroadProbs["us_politics"] = 0.9
		}
		out[u] = p
	}
	return out, nil
}

func (s *politicsEveryOther) ViewerFilters(_ context.Context, did, rkey string) (*Filters, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.saved[did+"/"+rkey], nil
}

func (s *politicsEveryOther) SaveViewerFilters(_ context.Context, did, rkey string, fl *Filters) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.saved == nil {
		s.saved = map[string]*Filters{}
	}
	s.saved[did+"/"+rkey] = fl
	return nil
}

// fakeTokens signs in only the viewers listed; a token names the viewer, audience and method.
type fakeTokens struct{ connected map[string]bool }

func (f fakeTokens) Token(_ context.Context, did, aud, lxm string) (string, error) {
	if !f.connected[did] {
		return "", signin.ErrNotConnected
	}
	return did + "|" + aud + "|" + lxm, nil
}

type filteredSetup struct {
	srv    *Server
	source *fakeSourceFeed
	ff     *FilteredFeeds
	viewer *testViewer
}

func newFilteredSetup(t *testing.T, total int) *filteredSetup {
	t.Helper()
	src := &fakeSourceFeed{total: total, got: make(chan struct{}, 10)}
	ts := httptest.NewServer(src)
	t.Cleanup(ts.Close)
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	sources := &Sources{Log: log, found: map[string]FeedSource{testSourceURI: {URI: testSourceURI,
		ServiceDID: "did:web:source.test", Endpoint: ts.URL, AcceptsInteractions: true, DisplayName: "Hot"}}}
	viewer := newTestViewer(t, "did:plc:viewer")
	store := &politicsEveryOther{}
	ff := NewFilteredFeeds(sources, store, fakeTokens{connected: map[string]bool{viewer.did: true}}, log)
	ff.LeftOut = store
	ff.SignInPost = "at://did:plc:owner/app.bsky.feed.post/signin"
	cfg := &Config{Feeds: []Feed{filteredFeed()}}
	fs := NewFeeds(cfg, fakeBuilder{}, log, 48*time.Hour, time.Hour, 100)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	fs.Start(ctx)
	srv := NewServer(ServerConfig{Hostname: "feeds.example.com", ServiceDID: "did:web:feeds.example.com",
		OwnerDID: syntax.DID("did:plc:owner"), MaxAge: time.Minute}, fs, testDirectory(viewer.doc()), log)
	srv.Filtered = ff
	return &filteredSetup{srv: srv, source: src, ff: ff, viewer: viewer}
}

type filteredSkel struct {
	Feed []struct {
		Post        string          `json:"post"`
		Reason      json.RawMessage `json:"reason"`
		FeedContext string          `json:"feedContext"`
	} `json:"feed"`
	Cursor string `json:"cursor"`
	ReqID  string `json:"reqId"`
}

func (s *filteredSetup) page(t *testing.T, cursor string, limit int, auth string) (int, filteredSkel) {
	t.Helper()
	q := url.Values{"feed": {"at://did:plc:owner/app.bsky.feed.generator/hot-filter"}, "limit": {strconv.Itoa(limit)}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	req := httptest.NewRequest(http.MethodGet, "/xrpc/app.bsky.feed.getFeedSkeleton?"+q.Encode(), nil)
	if auth != "" {
		req.Header.Set("Authorization", "Bearer "+auth)
	}
	req.Header.Set("Accept-Language", "en,de")
	rec := httptest.NewRecorder()
	s.srv.Handler().ServeHTTP(rec, req)
	var sk filteredSkel
	json.Unmarshal(rec.Body.Bytes(), &sk)
	return rec.Code, sk
}

func (s *filteredSetup) credential(t *testing.T) string {
	return s.viewer.credential(t, "did:web:feeds.example.com", skeletonMethod)
}

func TestFilteredFeedShowsOnlyTheSignInPostWithoutASignIn(t *testing.T) {
	s := newFilteredSetup(t, 50)
	// No credential at all, then a viewer who never signed in for the filtered feeds.
	stranger := newTestViewer(t, "did:plc:stranger")
	s.srv.dir = testDirectory(s.viewer.doc(), stranger.doc())
	for name, auth := range map[string]string{"no credential": "", "not signed in": stranger.credential(t, "did:web:feeds.example.com", skeletonMethod)} {
		code, sk := s.page(t, "", 30, auth)
		if code != 200 || len(sk.Feed) != 1 || sk.Feed[0].Post != s.ff.SignInPost || sk.Cursor != "" {
			t.Errorf("%s: %d %+v", name, code, sk)
		}
	}
	if len(s.source.pages) != 0 {
		t.Errorf("the source was asked %d times for viewers without a sign-in", len(s.source.pages))
	}
}

func TestFilteredFeedFillsThePageAndHoldsTheRest(t *testing.T) {
	s := newFilteredSetup(t, 95)
	auth := s.credential(t)
	// Half the posts are politics, so 25 kept takes two source pages of 30, which keep 30: five
	// are held over for the next page.
	code, sk := s.page(t, "", 25, auth)
	if code != 200 || len(sk.Feed) != 25 || sk.Cursor == "" {
		t.Fatalf("page 1: %d, %d posts, cursor %q", code, len(sk.Feed), sk.Cursor)
	}
	if len(s.source.pages) < 2 {
		t.Errorf("read %d source pages for a page whose filters leave out half", len(s.source.pages))
	}
	for _, it := range sk.Feed {
		n, _ := strconv.Atoi(it.Post[strings.LastIndex(it.Post, "/p")+2:])
		if n%2 == 0 {
			t.Errorf("a politics post came through: %s", it.Post)
		}
	}
	if string(sk.Feed[0].Reason) == "" || !strings.Contains(string(sk.Feed[0].Reason), "skeletonReasonRepost") {
		t.Errorf("p1's repost reason was not passed on: %s", sk.Feed[0].Reason)
	}
	if id, ctx := unwrapContext(sk.Feed[0].FeedContext); id != "req-0" || ctx != "ctx-1" {
		t.Errorf("p1's feed context: %q %q", id, ctx)
	}
	if s.source.auths[0] != "Bearer did:plc:viewer|did:web:source.test#bsky_fg|app.bsky.feed.getFeedSkeleton" {
		t.Errorf("the source got %q", s.source.auths[0])
	}
	if s.source.langs[0] != "en,de" || s.source.pages[0].Get("feed") != testSourceURI {
		t.Errorf("the source request: %v %q", s.source.pages[0], s.source.langs[0])
	}

	// The rest, page by page: every odd post once, in order, and no politics.
	seen := map[string]bool{}
	for _, it := range sk.Feed {
		seen[it.Post] = true
	}
	cursor := sk.Cursor
	for i := 0; cursor != "" && i < 10; i++ {
		code, sk = s.page(t, cursor, 25, auth)
		if code != 200 || len(sk.Feed) > 25 {
			t.Fatalf("page %d: %d", i+2, code)
		}
		for _, it := range sk.Feed {
			if seen[it.Post] {
				t.Errorf("%s came twice", it.Post)
			}
			seen[it.Post] = true
		}
		cursor = sk.Cursor
	}
	if len(seen) != 47 { // p1, p3, ..., p93
		t.Errorf("%d posts in all, want the 47 odd ones", len(seen))
	}
}

func TestFilteredFeedRecordsWhatItLeavesOutForTheViewer(t *testing.T) {
	s := newFilteredSetup(t, 20)
	s.page(t, "", 10, s.credential(t))
	store := s.ff.Store.(*politicsEveryOther)
	store.mu.Lock()
	rows := slices.Clone(store.leftOut)
	store.mu.Unlock()
	if len(rows) == 0 {
		t.Fatal("nothing recorded")
	}
	for _, r := range rows {
		n, _ := strconv.Atoi(r.URI[strings.LastIndex(r.URI, "/p")+2:])
		if r.ViewerDID != s.viewer.did || r.Feed != "hot-filter" || n%2 != 0 ||
			r.Reason != "topic" || r.Name != "us_politics" || r.Value != 0.9 || r.Cutoff != 0.5 || r.Bound != "max" {
			t.Errorf("recorded %+v", r)
		}
	}
	// Viewers without a sign-in leave nothing behind.
	before := len(rows)
	s.page(t, "", 10, "")
	if store.leftOut != nil && len(store.leftOut) != before {
		t.Error("a request without a viewer recorded posts")
	}
}

func TestFiltersSayWhyTheyLeaveAPostOut(t *testing.T) {
	fl := Filters{Exclude: map[string]float32{"us_politics": 0.5},
		Signals:    Rules{Max: map[string]float32{"critical": 0.6}},
		Tone:       Rules{Min: map[string]float32{"informative": 0.2}},
		TopicRules: map[string]TopicRules{"technology": {Signals: Rules{Max: map[string]float32{"critical": 0.3}}}}}
	cases := []struct {
		p    ScoredPost
		want *LeftOutReason
	}{
		{ScoredPost{}, nil},
		{ScoredPost{Scored: true, BroadProbs: map[string]float32{"us_politics": 0.7}}, &LeftOutReason{Kind: "topic", Name: "us_politics", Value: 0.7, Cutoff: 0.5, Bound: "max"}},
		{ScoredPost{Scored: true, Tone: map[string]float32{"informative": 0.1}}, &LeftOutReason{Kind: "tone", Name: "informative", Value: 0.1, Cutoff: 0.2, Bound: "min"}},
		{ScoredPost{Scored: true, Tone: map[string]float32{"informative": 0.5}, Signals: map[string]float32{"critical": 0.4}, TopPath: "technology/ai"},
			&LeftOutReason{Kind: "signal", Name: "critical", Value: 0.4, Cutoff: 0.3, Bound: "max", Rule: "technology"}},
		{ScoredPost{Scored: true, Tone: map[string]float32{"informative": 0.5}, Signals: map[string]float32{"critical": 0.4}}, nil},
	}
	for i, c := range cases {
		got := fl.Why(c.p)
		if (got == nil) != (c.want == nil) || (got != nil && *got != *c.want) {
			t.Errorf("case %d: %+v, want %+v", i, got, c.want)
		}
	}
	fl.DropUnscored = true
	if got := fl.Why(ScoredPost{}); got == nil || got.Kind != "unscored" {
		t.Errorf("unscored with drop_unscored: %+v", got)
	}
}

func TestFilteredFeedRetryOfACursorGetsTheSamePosts(t *testing.T) {
	s := newFilteredSetup(t, 200)
	auth := s.credential(t)
	_, first := s.page(t, "", 10, auth)
	_, a := s.page(t, first.Cursor, 10, auth)
	_, b := s.page(t, first.Cursor, 10, auth)
	if len(a.Feed) != 10 || len(b.Feed) != 10 || a.Feed[0].Post != b.Feed[0].Post {
		t.Errorf("the same cursor twice: %v then %v", a.Feed, b.Feed)
	}
}

func TestFilteredFeedUnscoredPosts(t *testing.T) {
	s := newFilteredSetup(t, 130) // p101 and after are never scored
	auth := s.credential(t)
	all := func() int {
		n, cursor := 0, ""
		for i := 0; i < 20; i++ {
			_, sk := s.page(t, cursor, 50, auth)
			n += len(sk.Feed)
			if cursor = sk.Cursor; cursor == "" {
				break
			}
		}
		return n
	}
	if n := all(); n != 50+29 { // the 50 odd posts to p99, and p101-p129 kept unjudged
		t.Errorf("kept %d with unscored posts kept, want 79", n)
	}
	s.ff.Store.SaveViewerFilters(context.Background(), s.viewer.did, "hot-filter",
		&Filters{Exclude: map[string]float32{"us_politics": 0.5}, DropUnscored: true})
	s.ff.Forget(s.viewer.did, "hot-filter")
	if n := all(); n != 50 {
		t.Errorf("kept %d with drop_unscored, want 50", n)
	}
}

func TestFilteredFeedSourceFailure(t *testing.T) {
	s := newFilteredSetup(t, 50)
	s.source.status = http.StatusUnauthorized
	code, _ := s.page(t, "", 30, s.credential(t))
	if code != http.StatusBadGateway {
		t.Errorf("a source that refuses the token: %d, want 502", code)
	}
}

func TestFilteredFeedSendsInteractionsOnAsTheViewer(t *testing.T) {
	s := newFilteredSetup(t, 50)
	_, sk := s.page(t, "", 10, s.credential(t))
	body, _ := json.Marshal(map[string]any{
		"feed": "at://did:plc:owner/app.bsky.feed.generator/hot-filter",
		"interactions": []map[string]string{
			{"item": sk.Feed[0].Post, "event": "app.bsky.feed.defs#interactionSeen", "feedContext": sk.Feed[0].FeedContext, "reqId": sk.ReqID},
			{"item": s.ff.SignInPost, "event": "app.bsky.feed.defs#interactionSeen", "reqId": sk.ReqID}, // not the source's
		},
	})
	req := httptest.NewRequest(http.MethodPost, "/xrpc/app.bsky.feed.sendInteractions", strings.NewReader(string(body)))
	req.Header.Set("Authorization", "Bearer "+s.viewer.credential(t, "did:web:feeds.example.com", interactionsMethod))
	req.Header.Set("Content-Type", "application/json")
	rec := httptest.NewRecorder()
	s.srv.Interactions = discardSink{}
	s.srv.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("sendInteractions: %d %s", rec.Code, rec.Body)
	}
	select {
	case <-s.source.got:
	case <-time.After(5 * time.Second):
		t.Fatal("the source never got the interactions")
	}
	s.source.mu.Lock()
	defer s.source.mu.Unlock()
	got := s.source.interactions[0]
	its := got["interactions"].([]any)
	if got["feed"] != testSourceURI || len(its) != 1 {
		t.Fatalf("the source got %v", got)
	}
	it := its[0].(map[string]any)
	if it["item"] != sk.Feed[0].Post || it["feedContext"] != "ctx-1" || it["reqId"] != "req-0" || it["event"] != "app.bsky.feed.defs#interactionSeen" {
		t.Errorf("the interaction sent on: %v", it)
	}
	if s.source.interAuth[0] != "Bearer did:plc:viewer|did:web:source.test#bsky_fg|app.bsky.feed.sendInteractions" {
		t.Errorf("sent on with %q", s.source.interAuth[0])
	}
}

func out2name(t *testing.T, do func(method, target, body string) (int, map[string]any)) string {
	t.Helper()
	_, out := do(http.MethodGet, "/api/me/filtered/hot-filter/left-out", "")
	name, _ := out["name"].(string)
	return name
}

type discardSink struct{}

func (discardSink) Add([]InteractionRow) int { return 0 }

func TestFilteredAPISavesAViewersFilters(t *testing.T) {
	s := newFilteredSetup(t, 10)
	api := &FilteredAPI{
		Viewer:    func(*http.Request) (string, bool) { return "did:plc:viewer", true },
		Connected: func(context.Context, string) (bool, error) { return true, nil },
		Feeds:     s.srv.feeds, Sources: s.ff.Sources, Store: s.ff.Store, Service: s.ff, OwnerDID: "did:plc:owner",
		Paths: map[string]bool{"us_politics": true, "technology": true}, Origin: "https://feeds.example.com",
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	s.srv.FilteredAPI = api
	do := func(method, target, body string) (int, map[string]any) {
		req := httptest.NewRequest(method, target, strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Origin", "https://feeds.example.com")
		rec := httptest.NewRecorder()
		s.srv.Handler().ServeHTTP(rec, req)
		var out map[string]any
		json.Unmarshal(rec.Body.Bytes(), &out)
		return rec.Code, out
	}
	code, list := do(http.MethodGet, "/api/me/filtered", "")
	feeds, _ := list["feeds"].([]any)
	if code != 200 || list["connected"] != true || len(feeds) != 1 {
		t.Fatalf("list: %d %v", code, list)
	}
	f := feeds[0].(map[string]any)
	if f["filters"] != nil || f["sourceName"] != "Hot" || f["sourceUrl"] != "https://bsky.app/profile/did:plc:source/feed/hot" {
		t.Errorf("the feed: %v", f)
	}
	for body, want := range map[string]int{
		`{"signals":{"max":{"critical":0.5}}}`:         200,
		`{"tone":{"weights":{"outraged":-2}}}`:         400,
		`{"exclude":{"nope":0.5}}`:                     400,
		`{"exclude":{"us_politics":0.5},"extra":true}`: 400,
		`null`: 200,
	} {
		if code, out := do(http.MethodPut, "/api/me/filtered/hot-filter", body); code != want {
			t.Errorf("save %s: %d %v, want %d", body, code, out, want)
		}
	}
	if code, _ := do(http.MethodPut, "/api/me/filtered/nope", `{}`); code != 404 {
		t.Errorf("a feed that isn't filtered: %d", code)
	}
	// What was left out of the viewer's own feed, and nobody else's.
	store := s.ff.Store.(*politicsEveryOther)
	store.Add([]LeftOutRow{
		{ViewerDID: "did:plc:viewer", Feed: "hot-filter", URI: "at://did:plc:a/app.bsky.feed.post/mine", Reason: "topic", Name: "adult_content"},
		{ViewerDID: "did:plc:someone", Feed: "hot-filter", URI: "at://did:plc:a/app.bsky.feed.post/theirs", Reason: "topic", Name: "adult_content"},
	})
	code, left := do(http.MethodGet, "/api/me/filtered/hot-filter/left-out", "")
	posts, _ := left["posts"].([]any)
	if code != 200 || len(posts) != 1 || posts[0].(map[string]any)["uri"] != "at://did:plc:a/app.bsky.feed.post/mine" {
		t.Errorf("left out: %d %v", code, left)
	}
	if code, _ := do(http.MethodGet, "/api/me/filtered/nope/left-out", ""); code != 404 {
		t.Errorf("left out of a feed that isn't filtered: %d", code)
	}
	if code, _ := do(http.MethodGet, "/api/me/filtered/hot-filter/left-out?limit=5000", ""); code != 400 {
		t.Errorf("a limit over 200: %d", code)
	}
	if code, _ := do(http.MethodGet, "/api/me/filtered/hot-filter/left-out?cursor=nonsense", ""); code != 400 {
		t.Errorf("a cursor that isn't one: %d", code)
	}
	// Page by page, with the cursor each page gives, to the end.
	for i := 0; i < 5; i++ {
		store.Add([]LeftOutRow{{ViewerDID: "did:plc:viewer", Feed: "hot-filter", URI: fmt.Sprintf("at://did:plc:a/app.bsky.feed.post/more%d", i),
			Reason: "topic", Name: "technology", LeftOutAt: time.Now()}})
	}
	seen, cursor := map[string]bool{}, ""
	for page := 0; page < 10; page++ {
		_, out := do(http.MethodGet, "/api/me/filtered/hot-filter/left-out?limit=2&cursor="+url.QueryEscape(cursor), "")
		for _, p := range out["posts"].([]any) {
			u := p.(map[string]any)["uri"].(string)
			if seen[u] {
				t.Errorf("%s twice", u)
			}
			seen[u] = true
		}
		if cursor, _ = out["cursor"].(string); cursor == "" {
			break
		}
	}
	if len(seen) != 6 || out2name(t, do) != "Hot, filtered" {
		t.Errorf("paged through %d posts, want 6 (the viewer's own)", len(seen))
	}
	do(http.MethodPut, "/api/me/filtered/hot-filter", `{"exclude":{"technology":0.3}}`)
	got, err := s.ff.viewerFilters(context.Background(), "did:plc:viewer", filteredFeed())
	if err != nil || got.Exclude["technology"] != 0.3 || got.Exclude["us_politics"] != 0 {
		t.Errorf("the feed doesn't use the saved filters: %+v %v", got, err)
	}
}

func TestFilteredTokenErrorIsAServerError(t *testing.T) {
	s := newFilteredSetup(t, 10)
	s.ff.Tokens = errTokens{}
	code, _ := s.page(t, "", 10, s.credential(t))
	if code != http.StatusInternalServerError {
		t.Errorf("a viewer's server that is down: %d", code)
	}
}

type errTokens struct{}

func (errTokens) Token(context.Context, string, string, string) (string, error) {
	return "", errors.New("their server is down")
}
