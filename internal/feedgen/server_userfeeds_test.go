package feedgen

import (
	"context"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	atmosidentity "github.com/jcalabro/atmos/identity"
	"github.com/prometheus/client_golang/prometheus"
)

// userFeedsServer serves the owner's feed "nfl" and two people's feeds that share the rkey "cats".
func userFeedsServer(t *testing.T) *Server { return userFeedsServerWith(t, testDirectory()) }

func userFeedsServerWith(t *testing.T, dir *atmosidentity.Directory) *Server {
	t.Helper()
	nfl := Feed{Rkey: "nfl", DisplayName: "NFL", Paths: []string{"sports/american_football"}, MinProb: 0.5, Ranking: DefaultRanking}
	bobs := personFeed("did:plc:bob", "cats")
	bobs.DisplayName = "Bob's cats"
	carols := personFeed("did:plc:carol", "cats")
	carols.DisplayName = "Carol's cats"
	fs := NewFeeds(&Config{Feeds: []Feed{nfl, bobs, carols}}, fakeBuilder{posts(5)}, quietLog, 48*time.Hour, time.Hour, 100)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	fs.Start(ctx)
	return NewServer(ServerConfig{Hostname: "feeds.example.com", ServiceDID: "did:web:feeds.example.com",
		OwnerDID: syntax.DID("did:plc:owner"), MaxAge: time.Minute}, fs, dir, quietLog)
}

func skeleton(t *testing.T, s *Server, feedURI string, extra ...string) (int, map[string]any) {
	t.Helper()
	q := "/xrpc/app.bsky.feed.getFeedSkeleton?feed=" + url.QueryEscape(feedURI)
	for _, e := range extra {
		q += "&" + e
	}
	return get(t, s, q)
}

func TestAFeedInSomeoneElsesRepoIsServed(t *testing.T) {
	s := userFeedsServer(t)
	code, body := skeleton(t, s, "at://did:plc:bob/app.bsky.feed.generator/cats", "limit=2")
	if code != 200 || len(body["feed"].([]any)) != 2 || body["cursor"] == nil {
		t.Fatalf("%d %v", code, body)
	}
	// The request ID names the kind of feed, not whose: a DID is not a rkey-dash-hex.
	if id := body["reqId"].(string); !strings.HasPrefix(id, "user-") || strings.Contains(id, "did:") || strings.Contains(id, "/") {
		t.Errorf("reqId %q", id)
	}
	// The next page continues from the cursor.
	code, next := skeleton(t, s, "at://did:plc:bob/app.bsky.feed.generator/cats", "limit=2", "cursor="+url.QueryEscape(body["cursor"].(string)))
	if code != 200 || len(next["feed"].([]any)) != 2 {
		t.Errorf("page 2: %d %v", code, next)
	}
	// The owner's own feed is served as before, with its rkey in the request ID.
	code, own := skeleton(t, s, "at://did:plc:owner/app.bsky.feed.generator/nfl")
	if code != 200 || !strings.HasPrefix(own["reqId"].(string), "nfl-") {
		t.Errorf("the owner's feed: %d %v", code, own)
	}
}

func TestTwoAccountsFeedsWithTheSameRkeyAreDifferentFeeds(t *testing.T) {
	s := userFeedsServer(t)
	for _, did := range []string{"did:plc:bob", "did:plc:carol"} {
		if code, body := skeleton(t, s, "at://"+did+"/app.bsky.feed.generator/cats"); code != 200 || len(body["feed"].([]any)) != 5 {
			t.Errorf("%s: %d %v", did, code, body)
		}
	}
	bob, _, _ := s.feeds.Posts("did:plc:bob/cats")
	carol, _, _ := s.feeds.Posts("did:plc:carol/cats")
	if len(bob) == 0 || len(carol) == 0 {
		t.Fatal("both are built")
	}
	if f, _ := s.feeds.Lookup("did:plc:bob/cats"); f.DisplayName != "Bob's cats" {
		t.Errorf("bob's: %+v", f)
	}
	if f, _ := s.feeds.Lookup("did:plc:carol/cats"); f.DisplayName != "Carol's cats" {
		t.Errorf("carol's: %+v", f)
	}
	// The owner has no feed with that rkey: a feed with someone's rkey in the owner's repo is not theirs.
	if code, body := skeleton(t, s, "at://did:plc:owner/app.bsky.feed.generator/cats"); code != 400 || body["error"] != "UnknownFeed" {
		t.Errorf("the owner has no 'cats': %d %v", code, body)
	}
}

func TestFeedsThatAreNotServedAreRefusedWhoseverRepoTheyAreIn(t *testing.T) {
	s := userFeedsServer(t)
	for name, uri := range map[string]string{
		"an account with no feeds":     "at://did:plc:dave/app.bsky.feed.generator/cats",
		"a feed of bob's that isn't":   "at://did:plc:bob/app.bsky.feed.generator/dogs",
		"another kind of record":       "at://did:plc:bob/app.bsky.feed.post/cats",
		"a handle instead of a DID":    "at://bob.example.com/app.bsky.feed.generator/cats",
		"no record key":                "at://did:plc:bob/app.bsky.feed.generator",
		"not an address":               "cats",
		"nothing":                      "",
		"a key made to look like ours": "at://did:plc:bob/app.bsky.feed.generator/did:plc:carol%2Fcats",
	} {
		code, body := skeleton(t, s, uri)
		if code != 400 || body["error"] != "UnknownFeed" {
			t.Errorf("%s: %d %v", name, code, body)
		}
	}
}

// What a stranger sends must not make up metric labels: one series per feed they name would grow
// without bound.
func TestMetricsNeverNameSomeoneElsesFeed(t *testing.T) {
	s := userFeedsServer(t)
	skeleton(t, s, "at://did:plc:bob/app.bsky.feed.generator/cats")
	skeleton(t, s, "at://did:plc:carol/app.bsky.feed.generator/cats", "limit=500") // an error that is labelled too
	skeleton(t, s, "at://did:plc:nobody/app.bsky.feed.generator/whatever")
	skeleton(t, s, "at://did:plc:owner/app.bsky.feed.generator/nfl")

	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	seen := map[string]bool{}
	for _, fam := range families {
		if !strings.HasPrefix(fam.GetName(), "feedgen_") {
			continue
		}
		for _, m := range fam.GetMetric() {
			for _, l := range m.GetLabel() {
				v := l.GetValue()
				seen[fam.GetName()+"{"+l.GetName()+"="+v+"}"] = true
				if strings.Contains(v, "did:") || strings.Contains(v, "whatever") {
					t.Errorf("%s{%s=%q} names a feed of someone else's", fam.GetName(), l.GetName(), v)
				}
			}
		}
	}
	if !seen["feedgen_skeleton_requests_total{feed=user}"] || !seen["feedgen_skeleton_requests_total{feed=nfl}"] || !seen["feedgen_skeleton_requests_total{feed=unknown}"] {
		t.Errorf("the owner's feed is named, everyone else's is 'user', and a feed that isn't there is 'unknown': %v", seen)
	}
}

func TestInteractionsAreKeptOnlyForFeedsWeServe(t *testing.T) {
	viewer := newTestViewer(t, "did:plc:viewer")
	s := userFeedsServerWith(t, testDirectory(viewer.doc()))
	sink := &fakeSink{}
	s.Interactions = sink
	send := func(feed string) {
		t.Helper()
		body := `{"feed":"` + feed + `","interactions":[{"item":"at://did:plc:a/app.bsky.feed.post/1","event":"app.bsky.feed.defs#interactionSeen","reqId":"user-abc"}]}`
		req := httptest.NewRequest(http.MethodPost, "/xrpc/app.bsky.feed.sendInteractions", strings.NewReader(body))
		req.Header.Set("Content-Type", "application/json")
		req.Header.Set("Authorization", "Bearer "+viewer.credential(t, "did:web:feeds.example.com", interactionsMethod))
		rec := httptest.NewRecorder()
		s.Handler().ServeHTTP(rec, req)
		if rec.Code != 200 {
			t.Fatalf("%s: %d", feed, rec.Code)
		}
	}
	send("at://did:plc:bob/app.bsky.feed.generator/cats")        // someone else's, served
	send("at://did:plc:owner/app.bsky.feed.generator/nfl")       // the owner's
	send("at://did:plc:nobody/app.bsky.feed.generator/invented") // not served
	send("at://did:plc:owner/app.bsky.feed.generator/invented")  // not served either, though in the owner's repo
	var got []string
	for _, r := range sink.rows {
		got = append(got, r.Feed)
	}
	if strings.Join(got, "|") != "did:plc:bob/cats|nfl||" {
		t.Errorf("feeds on the stored interactions: %q", got)
	}
	// ... and the metric names the owner's feed, and everyone else's as one.
	families, err := prometheus.DefaultGatherer.Gather()
	if err != nil {
		t.Fatal(err)
	}
	labels := map[string]bool{}
	for _, fam := range families {
		if fam.GetName() != "feedgen_interactions_total" {
			continue
		}
		for _, m := range fam.GetMetric() {
			for _, l := range m.GetLabel() {
				if l.GetName() == "feed" {
					labels[l.GetValue()] = true
				}
			}
		}
	}
	for l := range labels {
		if strings.Contains(l, "did:") || strings.Contains(l, "invented") {
			t.Errorf("the interactions metric has the label %q", l)
		}
	}
	if !labels["user"] || !labels["nfl"] || !labels["unknown"] {
		t.Errorf("feed labels of the interactions metric: %v", labels)
	}
}
