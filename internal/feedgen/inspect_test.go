package feedgen

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/haileyok/topic-feed/internal/labelpolicy"
)

const (
	inspectOwner = "did:plc:owner"
	inspectKey   = "3mwxzusywpk2f"
	inspectDID   = "did:plc:a"
	inspectURI   = "at://did:plc:a/app.bsky.feed.post/" + inspectKey
)

type perFeedBuilder map[string][]Post

func (b perFeedBuilder) Build(_ context.Context, f Feed, _ time.Time, _ int) ([]Post, Removed, error) {
	return b[f.Rkey], Removed{}, nil
}

type fakeInspectSource struct {
	facts PostFacts
	err   error
	asked []string
}

func (s *fakeInspectSource) PostFacts(_ context.Context, uri, did string) (PostFacts, error) {
	s.asked = append(s.asked, uri+" "+did)
	if s.err != nil {
		return PostFacts{}, s.err
	}
	f := s.facts
	f.URI, f.DID = uri, did
	return f, nil
}

type fakeRemote struct {
	post  RemotePost
	err   error
	asked []string
}

func (r *fakeRemote) Lookup(_ context.Context, uri string) (RemotePost, error) {
	r.asked = append(r.asked, uri)
	return r.post, r.err
}

type inspectRig struct {
	api      *InspectAPI
	src      *fakeInspectSource
	remote   *fakeRemote
	resolved []string
	resolve  func(string) (string, error)
}

// scoredFacts is a post the pipeline scored as about AI, which the AI feed holds.
func scoredFacts() PostFacts {
	in := aiPost()
	return PostFacts{
		Stored: &StoredPost{CID: "bafy", Text: "a post about models", Langs: []string{"en"}, EmbedType: "none", IndexedAt: in.Post.IndexedAt, SelfLabels: []string{}, Tags: []string{}, MediaKinds: []string{}},
		Pipeline: &PipelineRow{IndexedAt: in.Post.IndexedAt, ProcessedAt: in.Post.IndexedAt.Add(time.Minute), FeedPolicy: labelpolicy.OK, Labels: []string{}, Model: "v5",
			BroadProbs: in.BroadProbs, PathProbs: in.PathProbs, Signals: in.Post.Signals, Tone: in.Post.Tone, TopBroad: "technology", TopPath: "technology/ai", TopPathP: 0.8, PicturesWanted: 1, PicturesUsed: 1},
		Post: Post{URI: inspectURI, DID: inspectDID, IndexedAt: in.Post.IndexedAt, TopPath: "technology/ai", TopPathP: 0.8, Tone: in.Post.Tone, Signals: in.Post.Signals, Likes: 6, Reposts: 1},
	}
}

func newInspectRig(t *testing.T) *inspectRig {
	t.Helper()
	ai := aiFeed()
	sports := Feed{Rkey: "sports", DisplayName: "Sports", Paths: []string{"sports"}, MinProb: 0.5, Ranking: DefaultRanking}
	cfg := &Config{Feeds: []Feed{sports, ai, forYouFeed()}} // sports first, to see the order change
	target := Post{URI: inspectURI, DID: inspectDID, IndexedAt: inspectNow.Add(-time.Hour)}
	other := Post{URI: "at://did:plc:b/app.bsky.feed.post/2", DID: "did:plc:b", IndexedAt: inspectNow.Add(-2 * time.Hour)}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fs := NewFeeds(cfg, perFeedBuilder{"ai": {target, other}, "sports": {other}}, log, 24*time.Hour, time.Hour, 3000)
	for _, f := range cfg.Feeds {
		if f.Personal == nil {
			fs.buildNow(context.Background(), f)
		}
	}
	rig := &inspectRig{src: &fakeInspectSource{facts: scoredFacts()}, remote: &fakeRemote{}}
	rig.resolve = func(h string) (string, error) { return inspectDID, nil }
	rig.api = &InspectAPI{
		Viewer: func(r *http.Request) (string, bool) { d := r.Header.Get("X-Viewer"); return d, d != "" },
		Owner:  inspectOwner, Source: rig.src, Feeds: fs, Policy: testPolicy(),
		Names: map[string]TopicName{"technology/ai": {Name: "AI", Broad: "Technology"}, "technology": {Name: "Technology", Broad: "Technology"}},
		Resolve: func(_ context.Context, h string) (string, error) {
			rig.resolved = append(rig.resolved, h)
			return rig.resolve(h)
		},
		Handle: func(context.Context, string) string { return "alice.example.com" },
		Remote: rig.remote, Limit: NewIPLimiter(time.Hour, 50), Log: log, Now: func() time.Time { return inspectNow },
	}
	return rig
}

func (r *inspectRig) ask(t *testing.T, viewer, post string) (*httptest.ResponseRecorder, InspectResponse) {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, "/api/inspect?post="+url.QueryEscape(post), nil)
	if viewer != "" {
		req.Header.Set("X-Viewer", viewer)
	}
	w := httptest.NewRecorder()
	r.api.ServeInspect(w, req)
	var resp InspectResponse
	if w.Code == http.StatusOK {
		if err := json.Unmarshal(w.Body.Bytes(), &resp); err != nil {
			t.Fatalf("%v: %s", err, w.Body.String())
		}
	}
	return w, resp
}

func errorOf(t *testing.T, w *httptest.ResponseRecorder) (code, message string) {
	t.Helper()
	var e map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &e); err != nil {
		t.Fatalf("not an error body: %s", w.Body.String())
	}
	return e["error"], e["message"]
}

func TestInspectIsForTheOwnerOnly(t *testing.T) {
	r := newInspectRig(t)
	w, _ := r.ask(t, "", inspectURI)
	if code, _ := errorOf(t, w); w.Code != http.StatusUnauthorized || code != "not signed in" {
		t.Errorf("signed out: %d %s", w.Code, w.Body.String())
	}
	w, _ = r.ask(t, "did:plc:someoneelse", inspectURI)
	if code, _ := errorOf(t, w); w.Code != http.StatusForbidden || code != "forbidden" {
		t.Errorf("another account: %d %s", w.Code, w.Body.String())
	}
	if len(r.src.asked) != 0 || len(r.resolved) != 0 {
		t.Errorf("nothing is read for someone who may not ask: %v %v", r.src.asked, r.resolved)
	}
	if w.Header().Get("Cache-Control") != "no-store" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers: %v", w.Header())
	}
	w, _ = r.ask(t, inspectOwner, inspectURI)
	if w.Code != http.StatusOK {
		t.Errorf("the owner: %d %s", w.Code, w.Body.String())
	}
}

func TestInspectLimitsHowOftenTheOwnerAsks(t *testing.T) {
	r := newInspectRig(t)
	r.api.Limit = NewIPLimiter(time.Hour, 2)
	for i := 0; i < 2; i++ {
		if w, _ := r.ask(t, inspectOwner, inspectURI); w.Code != http.StatusOK {
			t.Fatalf("request %d: %d", i+1, w.Code)
		}
	}
	w, _ := r.ask(t, inspectOwner, inspectURI)
	if code, _ := errorOf(t, w); w.Code != http.StatusTooManyRequests || code != "limited" || w.Header().Get("Retry-After") == "" {
		t.Errorf("a third: %d %s %v", w.Code, w.Body.String(), w.Header())
	}
}

func TestInspectRefusesWhatIsNotAPost(t *testing.T) {
	r := newInspectRig(t)
	for _, in := range []string{"", "hello", "https://example.com/x", "at://did:plc:a/app.bsky.feed.like/" + inspectKey, "https://bsky.app/profile/alice.example.com"} {
		w, _ := r.ask(t, inspectOwner, in)
		code, msg := errorOf(t, w)
		if w.Code != http.StatusBadRequest || code != "invalid" || msg == "" {
			t.Errorf("%q: %d %s", in, w.Code, w.Body.String())
		}
	}
	if len(r.src.asked) != 0 {
		t.Errorf("nothing is read for an address that isn't a post: %v", r.src.asked)
	}
}

func TestInspectResolvesAHandleOnlyWhenItHasTo(t *testing.T) {
	r := newInspectRig(t)
	// A DID needs no lookup.
	if w, _ := r.ask(t, inspectOwner, inspectURI); w.Code != 200 || len(r.resolved) != 0 {
		t.Errorf("a DID: %d, resolved %v", w.Code, r.resolved)
	}
	// A handle is looked up, in its normal form, and the post is read under the DID it names.
	w, resp := r.ask(t, inspectOwner, "https://bsky.app/profile/Alice.Example.COM/post/"+inspectKey)
	if w.Code != 200 || len(r.resolved) != 1 || r.resolved[0] != "alice.example.com" {
		t.Fatalf("a handle: %d, resolved %v", w.Code, r.resolved)
	}
	if resp.URI != inspectURI || resp.DID != inspectDID || resp.Handle != "alice.example.com" {
		t.Errorf("the post is named by the DID the handle resolved to: %+v", resp)
	}
	if got := r.src.asked[len(r.src.asked)-1]; got != inspectURI+" "+inspectDID {
		t.Errorf("read: %s", got)
	}

	r.resolve = func(string) (string, error) { return "", ErrNoSuchHandle }
	w, _ = r.ask(t, inspectOwner, "https://bsky.app/profile/nobody.example.com/post/"+inspectKey)
	if code, msg := errorOf(t, w); w.Code != http.StatusNotFound || code != "no_such_account" || !strings.Contains(msg, "nobody.example.com") {
		t.Errorf("no such handle: %d %s", w.Code, w.Body.String())
	}
	r.resolve = func(string) (string, error) { return "", errors.New("dns is down") }
	w, _ = r.ask(t, inspectOwner, "https://bsky.app/profile/alice.example.com/post/"+inspectKey)
	if code, msg := errorOf(t, w); w.Code != http.StatusServiceUnavailable || code != "unavailable" || strings.Contains(msg, "dns") {
		t.Errorf("lookup failed: %d %s (what went wrong inside isn't sent)", w.Code, w.Body.String())
	}
	r.api.Resolve = nil
	w, _ = r.ask(t, inspectOwner, "https://bsky.app/profile/alice.example.com/post/"+inspectKey)
	if w.Code != http.StatusBadRequest {
		t.Errorf("no resolver: %d", w.Code)
	}
}

func TestInspectDatabaseTroubleIsNotRevealed(t *testing.T) {
	r := newInspectRig(t)
	r.src.err = errors.New("clickhouse: password for user default is wrong")
	w, _ := r.ask(t, inspectOwner, inspectURI)
	if code, msg := errorOf(t, w); w.Code != http.StatusServiceUnavailable || code != "unavailable" || strings.Contains(msg+w.Body.String(), "password") {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
}

func verdictOf(resp InspectResponse, rkey string) (FeedVerdict, bool) {
	for _, v := range resp.Feeds {
		if v.Rkey == rkey {
			return v, true
		}
	}
	return FeedVerdict{}, false
}

func TestInspectAScoredPost(t *testing.T) {
	r := newInspectRig(t)
	w, resp := r.ask(t, inspectOwner, "https://bsky.app/profile/"+inspectDID+"/post/"+inspectKey)
	if w.Code != 200 {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	if resp.State != "scored" || len(resp.Why) != 0 || resp.URL != "https://bsky.app/profile/"+inspectDID+"/post/"+inspectKey || resp.WindowHours != 24 {
		t.Errorf("state: %+v", resp)
	}
	p := resp.Post
	if p == nil || p.Text != "a post about models" || p.Topic != "AI" || p.Broad != "Technology" || p.TopicPath != "technology/ai" || p.Likes != 6 {
		t.Fatalf("post: %+v", p)
	}
	if len(p.Top) == 0 || p.Top[0].Path != "technology/ai" || p.Top[0].Name != "AI" || p.Top[0].P != 0.8 {
		t.Errorf("top topics: %+v", p.Top)
	}
	if len(p.Subtopics) != 2 || p.Subtopics[0].P < p.Subtopics[1].P || len(p.Broads) != 1 {
		t.Errorf("every score, best first: %+v %+v", p.Subtopics, p.Broads)
	}
	if p.Pipeline == nil || p.Pipeline.Model != "v5" || p.Pipeline.PicturesWanted != 1 || p.Tone["informative"] != 0.6 {
		t.Errorf("pipeline: %+v", p.Pipeline)
	}

	// Every configured feed gives a verdict; those that take the post come first and the personal feed last.
	var order []string
	for _, v := range resp.Feeds {
		order = append(order, v.Rkey)
	}
	if strings.Join(order, ",") != "ai,sports,for-you" {
		t.Errorf("order: %v", order)
	}
	ai, _ := verdictOf(resp, "ai")
	if !ai.Matches || !ai.Fresh || ai.Live == nil || !ai.Live.InBuild || ai.Live.Position < 1 || ai.Live.Total != 2 || ai.Ranking == nil || ai.Ranking.Score <= 0 {
		t.Errorf("the AI feed takes it and holds it: %+v live %+v", ai, ai.Live)
	}
	sports, _ := verdictOf(resp, "sports")
	if sports.Matches || sports.Reason == "" || sports.Live == nil || sports.Live.InBuild || sports.Live.Why != "" {
		t.Errorf("the sports feed doesn't: %+v live %+v", sports, sports.Live)
	}
	you, _ := verdictOf(resp, "for-you")
	if !you.Personal || !you.Matches || you.Live != nil || you.Note == "" {
		t.Errorf("for you: %+v", you)
	}
	if resp.Summary != (InspectSummary{Feeds: 2, Matching: 1, InNow: 1}) {
		t.Errorf("summary: %+v", resp.Summary)
	}
}

func TestInspectAPostThatMatchesButIsNotInTheBuild(t *testing.T) {
	r := newInspectRig(t)
	r.src.facts.Post.IndexedAt = inspectNow.Add(-90 * time.Minute)
	r.src.facts.Pipeline.IndexedAt = inspectNow.Add(-90 * time.Minute)
	r.src.facts.Post.URI = "at://did:plc:a/app.bsky.feed.post/other" // not what the build holds
	_, resp := r.ask(t, inspectOwner, "at://did:plc:a/app.bsky.feed.post/"+inspectKey+"x")
	ai, _ := verdictOf(resp, "ai")
	if !ai.Matches || ai.Live == nil || ai.Live.InBuild || ai.Live.Why == "" {
		t.Errorf("matches, isn't in the build, and says why: %+v live %+v", ai, ai.Live)
	}
	if resp.Summary.Matching != 1 || resp.Summary.InNow != 0 {
		t.Errorf("summary: %+v", resp.Summary)
	}
}

func TestInspectAnOldPostMatchedWhileFresh(t *testing.T) {
	r := newInspectRig(t)
	r.src.facts.Post.IndexedAt = inspectNow.Add(-72 * time.Hour)
	r.src.facts.Pipeline.IndexedAt = inspectNow.Add(-72 * time.Hour)
	_, resp := r.ask(t, inspectOwner, "at://did:plc:a/app.bsky.feed.post/oldone")
	ai, _ := verdictOf(resp, "ai")
	if !ai.Matches || ai.Fresh || ai.Live == nil || !strings.Contains(ai.Live.Why, "older than the 24h0m0s") {
		t.Errorf("an old post would have made it, but is past the window: %+v live %+v", ai, ai.Live)
	}
}

func TestInspectAPostThatWasNotScored(t *testing.T) {
	r := newInspectRig(t)
	// Dropped by the label policy: processed, no model.
	r.src.facts.Pipeline.Model, r.src.facts.Pipeline.FeedPolicy, r.src.facts.Pipeline.Labels = "", labelpolicy.Drop, []string{"csam"}
	_, resp := r.ask(t, inspectOwner, inspectURI)
	if resp.State != "unscored" || len(resp.Why) != 2 || !strings.Contains(resp.Why[1], "\"drop\" (labels: csam)") {
		t.Errorf("dropped: %+v", resp)
	}
	if v, _ := verdictOf(resp, "ai"); v.Matches {
		t.Errorf("a post that was never scored matches nothing: %+v", v)
	}
	// Not dropped, just nothing to classify.
	r.src.facts.Pipeline.FeedPolicy, r.src.facts.Pipeline.Labels = labelpolicy.OK, nil
	_, resp = r.ask(t, inspectOwner, inspectURI)
	if resp.State != "unscored" || !strings.Contains(resp.Why[1], "nothing to classify") {
		t.Errorf("no model, not dropped: %+v", resp.Why)
	}
	// Stored, never processed.
	r.src.facts.Pipeline = nil
	_, resp = r.ask(t, inspectOwner, inspectURI)
	if resp.State != "unprocessed" || len(resp.Feeds) != 0 || resp.Post == nil || resp.Post.Text == "" || len(resp.Why) != 2 {
		t.Errorf("stored but not processed: %+v", resp)
	}
}

func TestInspectAPostWeDoNotHold(t *testing.T) {
	r := newInspectRig(t)
	r.src.facts = PostFacts{}
	for _, c := range []struct {
		name   string
		facts  func(*PostFacts)
		remote RemotePost
		err    error
		noRem  bool
		want   string
	}{
		{name: "a reply", remote: RemotePost{Found: true, IsReply: true, Langs: []string{"en"}}, want: "This is a reply. Replies aren't stored"},
		{name: "another language", remote: RemotePost{Found: true, Langs: []string{"ja"}}, want: "tagged ja, not English"},
		{name: "no language tag", remote: RemotePost{Found: true}, want: "no language tag"},
		{name: "should have been kept", remote: RemotePost{Found: true, Langs: []string{"en-US"}}, want: "should have been kept"},
		{name: "gone from Bluesky", remote: RemotePost{Found: false}, want: "Bluesky doesn't return it either"},
		{name: "Bluesky can't be asked, but we saw it", err: errors.New("timeout"), facts: func(f *PostFacts) { f.SeenText = true }, want: "we keep the text of every post for a week"},
		{name: "Bluesky can't be asked, and we never saw it", err: errors.New("timeout"), want: "no record of seeing it"},
		{name: "no way to ask Bluesky", noRem: true, want: "We don't hold this post"},
	} {
		t.Run(c.name, func(t *testing.T) {
			r.src.facts = PostFacts{}
			if c.facts != nil {
				c.facts(&r.src.facts)
			}
			r.remote.post, r.remote.err = c.remote, c.err
			if c.noRem {
				r.api.Remote = nil
			} else {
				r.api.Remote = r.remote
			}
			_, resp := r.ask(t, inspectOwner, inspectURI)
			if resp.State != "not_stored" || resp.Post != nil || len(resp.Feeds) != 0 {
				t.Fatalf("a post we don't hold has no scores and no verdicts: %+v", resp)
			}
			if !strings.Contains(strings.Join(resp.Why, " "), c.want) {
				t.Errorf("why: %v, want it to say %q", resp.Why, c.want)
			}
		})
	}
	if len(r.remote.asked) == 0 || r.remote.asked[0] != inspectURI {
		t.Errorf("Bluesky was asked about the post: %v", r.remote.asked)
	}
}

// Bluesky is asked only about a post we don't hold: a stored one says what it is itself.
func TestInspectDoesNotAskBlueskyAboutAPostItHolds(t *testing.T) {
	r := newInspectRig(t)
	r.ask(t, inspectOwner, inspectURI)
	if len(r.remote.asked) != 0 {
		t.Errorf("asked about a stored post: %v", r.remote.asked)
	}
}
