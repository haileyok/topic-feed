package feedgen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

const feedsOrigin = "https://feeds.example.com"

// feedsRig is a server with the feeds API over a store in memory. Who is signed in is the
// X-Viewer header, as in the inspector's tests.
type feedsRig struct {
	s     *Server
	api   *FeedsAPI
	store *countingFeedStore
	feeds *Feeds
}

// countingFeedStore is a fake store that counts how often it was asked and can be made to fail.
type countingFeedStore struct {
	*fakeFeedStore
	calls  atomic.Int32
	failOn string // "", "list", "save" or "delete"
}

func (c *countingFeedStore) ListFeeds(ctx context.Context) ([]StoredFeed, error) {
	c.calls.Add(1)
	return c.fakeFeedStore.ListFeeds(ctx)
}
func (c *countingFeedStore) OwnerFeeds(ctx context.Context, o string) ([]StoredFeed, error) {
	c.calls.Add(1)
	if c.failOn == "list" {
		return nil, errors.New("clickhouse: secret detail")
	}
	return c.fakeFeedStore.OwnerFeeds(ctx, o)
}
func (c *countingFeedStore) SaveFeed(ctx context.Context, f StoredFeed) error {
	c.calls.Add(1)
	if c.failOn == "save" {
		return errors.New("clickhouse: secret detail")
	}
	return c.fakeFeedStore.SaveFeed(ctx, f)
}
func (c *countingFeedStore) DeleteFeed(ctx context.Context, o, r string) error {
	c.calls.Add(1)
	if c.failOn == "delete" {
		return errors.New("clickhouse: secret detail")
	}
	return c.fakeFeedStore.DeleteFeed(ctx, o, r)
}

func newFeedsRig(t *testing.T) *feedsRig {
	t.Helper()
	_, paths := realConfig(t)
	nfl := Feed{Rkey: "nfl", DisplayName: "NFL", Paths: []string{"sports/american_football"}, MinProb: 0.5, Ranking: DefaultRanking}
	fs := NewFeeds(&Config{Feeds: []Feed{nfl}}, fakeBuilder{posts(5)}, quietLog, 48*time.Hour, time.Hour, 100)
	s := NewServer(ServerConfig{Hostname: "feeds.example.com", ServiceDID: "did:web:feeds.example.com",
		OwnerDID: syntax.DID("did:plc:owner"), MaxAge: time.Minute}, fs, testDirectory(), quietLog)
	store := &countingFeedStore{fakeFeedStore: &fakeFeedStore{}}
	api := &FeedsAPI{
		Viewer: func(r *http.Request) (string, bool) { d := r.Header.Get("X-Viewer"); return d, d != "" },
		Owner:  "did:plc:owner", ServiceDID: "did:web:feeds.example.com", Store: store, Feeds: fs, Paths: paths,
		Limits: UserLimits{MaxFeeds: 2, MaxPaths: 3, MaxExclude: 2, MaxPosts: 100}, Origin: feedsOrigin,
		Edits: NewIPLimiter(time.Hour, 1000), Log: quietLog,
	}
	s.FeedsAPI = api
	return &feedsRig{s: s, api: api, store: store, feeds: fs}
}

const catsJSON = `{"display_name":"Cats","description":"Cats, mostly.","paths":["animals_nature/cats"],"min_prob":0.6}`

type feedsCall struct {
	method, path, viewer, body string
	header                     map[string]string
}

func (r *feedsRig) do(t *testing.T, c feedsCall) (int, map[string]any, *httptest.ResponseRecorder) {
	t.Helper()
	var body io.Reader
	if c.body != "" {
		body = strings.NewReader(c.body)
	}
	req := httptest.NewRequest(c.method, c.path, body)
	if c.viewer != "" {
		req.Header.Set("X-Viewer", c.viewer)
	}
	if c.body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	for k, v := range c.header {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	r.s.Handler().ServeHTTP(rec, req)
	var out map[string]any
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return rec.Code, out, rec
}

func (r *feedsRig) save(t *testing.T, viewer, rkey, body string) (int, map[string]any) {
	t.Helper()
	code, out, _ := r.do(t, feedsCall{method: "PUT", path: "/api/me/feeds/" + rkey, viewer: viewer, body: body})
	return code, out
}

func (r *feedsRig) list(t *testing.T, viewer string) FeedsResponse {
	t.Helper()
	_, _, rec := r.do(t, feedsCall{method: "GET", path: "/api/me/feeds", viewer: viewer})
	var out FeedsResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatalf("%v: %s", err, rec.Body)
	}
	return out
}

func TestNobodySignedInCanNotReadOrChangeAnyFeed(t *testing.T) {
	r := newFeedsRig(t)
	for _, c := range []feedsCall{
		{method: "GET", path: "/api/me/feeds"},
		{method: "PUT", path: "/api/me/feeds/cats", body: catsJSON},
		{method: "DELETE", path: "/api/me/feeds/cats"},
	} {
		if code, body, _ := r.do(t, c); code != 401 || body["error"] != "not signed in" {
			t.Errorf("%s %s: %d %v", c.method, c.path, code, body)
		}
	}
	if n := r.store.calls.Load(); n != 0 {
		t.Errorf("the database was asked %d times for nobody", n)
	}
}

func TestTheFeedsRoutesAreOffWithoutTheAPI(t *testing.T) {
	r := newFeedsRig(t)
	r.s.FeedsAPI = nil
	for _, c := range []feedsCall{
		{method: "GET", path: "/api/me/feeds", viewer: "did:plc:bob"},
		{method: "PUT", path: "/api/me/feeds/cats", viewer: "did:plc:bob", body: catsJSON},
		{method: "DELETE", path: "/api/me/feeds/cats", viewer: "did:plc:bob"},
		{method: "GET", path: "/oauth/browser-client-metadata.json"},
		{method: "GET", path: "/xrpc/com.atproto.identity.resolveHandle?handle=a.example.com"},
	} {
		if code, _, _ := r.do(t, c); code != 404 {
			t.Errorf("%s %s: %d", c.method, c.path, code)
		}
	}
}

func TestAnAccountSeesOnlyItsOwnFeedsAndWhatItMayDo(t *testing.T) {
	r := newFeedsRig(t)
	if got := r.list(t, "did:plc:bob"); got.DID != "did:plc:bob" || got.Owner || len(got.Feeds) != 0 || got.Feeds == nil ||
		got.ServiceDID != "did:web:feeds.example.com" || got.Scope != BrowserScope {
		t.Fatalf("a new account: %+v", got)
	}
	r.save(t, "did:plc:bob", "cats", catsJSON)
	r.save(t, "did:plc:alice", "dogs", strings.ReplaceAll(catsJSON, "cats", "dogs"))
	bob, alice := r.list(t, "did:plc:bob"), r.list(t, "did:plc:alice")
	if len(bob.Feeds) != 1 || bob.Feeds[0].Rkey != "cats" || len(alice.Feeds) != 1 || alice.Feeds[0].Rkey != "dogs" {
		t.Errorf("each sees their own: %+v | %+v", bob.Feeds, alice.Feeds)
	}
	if bob.Feeds[0].URI != "at://did:plc:bob/app.bsky.feed.generator/cats" || bob.Feeds[0].Spec.DisplayName != "Cats" || bob.Feeds[0].CreatedAt.IsZero() {
		t.Errorf("a feed as shown: %+v", bob.Feeds[0])
	}
	if bob.Limits.MaxFeeds != 2 || bob.Limits.MaxPaths != 3 || bob.Limits.MaxExclude != 2 || bob.Limits.MaxPosts != 100 || bob.Limits.MaxName != 24 || bob.Limits.MaxDescription != 300 {
		t.Errorf("limits: %+v", bob.Limits)
	}
	if owner := r.list(t, "did:plc:owner"); !owner.Owner || owner.Limits.MaxFeeds != 0 || owner.Limits.MaxPaths != 0 || owner.Limits.MaxName != 24 {
		t.Errorf("the owner has no limits to show: %+v", owner)
	}
}

func TestSavingAFeedMakesItAndTheServiceIsToldToLookAgain(t *testing.T) {
	r := newFeedsRig(t)
	code, body := r.save(t, "did:plc:bob", "cats", catsJSON)
	feed, _ := body["feed"].(map[string]any)
	if code != 201 || body["created"] != true || feed["rkey"] != "cats" || feed["uri"] != "at://did:plc:bob/app.bsky.feed.generator/cats" {
		t.Fatalf("%d %v", code, body)
	}
	stored, _ := r.store.OwnerFeeds(context.Background(), "did:plc:bob")
	if len(stored) != 1 || stored[0].Owner != "did:plc:bob" || stored[0].Spec.MinProb != 0.6 || stored[0].Spec.Ranking != DefaultRanking || !stored[0].Spec.AcceptsInteractions {
		t.Fatalf("stored: %+v", stored)
	}
	if len(r.feeds.syncNow) != 1 {
		t.Error("the service is not told there is a feed to look at")
	}
	// Changing it replaces it, keeps when it began, and says so.
	created := stored[0].CreatedAt
	time.Sleep(2 * time.Millisecond)
	code, body = r.save(t, "did:plc:bob", "cats", strings.Replace(catsJSON, `"Cats"`, `"Cats and more"`, 1))
	if code != 200 || body["created"] != false {
		t.Fatalf("an edit: %d %v", code, body)
	}
	got := r.list(t, "did:plc:bob")
	if len(got.Feeds) != 1 || got.Feeds[0].Spec.DisplayName != "Cats and more" || !got.Feeds[0].CreatedAt.Equal(created) {
		t.Errorf("after the edit: %+v", got.Feeds)
	}
}

func TestOnlyTheSignedInAccountsOwnFeedCanBeChanged(t *testing.T) {
	r := newFeedsRig(t)
	r.save(t, "did:plc:bob", "cats", catsJSON)
	// The same rkey from another account is that account's own feed, not bob's.
	r.save(t, "did:plc:alice", "cats", strings.Replace(catsJSON, `"Cats"`, `"Alice's cats"`, 1))
	if got := r.list(t, "did:plc:bob"); got.Feeds[0].Spec.DisplayName != "Cats" {
		t.Errorf("alice changed bob's feed: %+v", got.Feeds[0])
	}
	// Nothing in the request names whose feed it is: a field that tries to is not accepted.
	for _, field := range []string{`"owner":"did:plc:bob"`, `"did":"did:plc:bob"`, `"owner_did":"did:plc:bob"`} {
		body := strings.Replace(catsJSON, "{", "{"+field+",", 1)
		if code, out := r.save(t, "did:plc:alice", "cats", body); code != 400 {
			t.Errorf("%s: %d %v", field, code, out)
		}
	}
	// Deleting takes away the signed-in account's feed and no other.
	if code, _, _ := r.do(t, feedsCall{method: "DELETE", path: "/api/me/feeds/cats", viewer: "did:plc:alice"}); code != 200 {
		t.Fatal(code)
	}
	if got := r.list(t, "did:plc:bob"); len(got.Feeds) != 1 {
		t.Errorf("alice deleted bob's feed: %+v", got.Feeds)
	}
	if got := r.list(t, "did:plc:alice"); len(got.Feeds) != 0 {
		t.Errorf("her own is gone: %+v", got.Feeds)
	}
	// Deleting one that isn't there is fine; what can't be the key of a feed is not.
	if code, _, _ := r.do(t, feedsCall{method: "DELETE", path: "/api/me/feeds/never-was", viewer: "did:plc:alice"}); code != 200 {
		t.Errorf("deleting nothing: %d", code)
	}
	calls := r.store.calls.Load()
	if code, out, _ := r.do(t, feedsCall{method: "DELETE", path: "/api/me/feeds/NOT_A_KEY", viewer: "did:plc:alice"}); code != 400 || out["error"] != "invalid" {
		t.Errorf("deleting a key that can't be one: %d %v", code, out)
	}
	if r.store.calls.Load() != calls {
		t.Error("the database was asked about a key that can't be one")
	}
}

func TestAFeedThatIsNotValidIsRefusedInWords(t *testing.T) {
	r := newFeedsRig(t)
	long := strings.Repeat("x", 25)
	for name, c := range map[string]struct{ rkey, body, want string }{
		"an rkey with capitals":           {"Cats", catsJSON, "The feed's key must be"},
		"an rkey that is too long":        {"abcdefghijklmnop", catsJSON, "The feed's key must be"},
		"an rkey that starts with a dash": {"-cats", catsJSON, "The feed's key must be"},
		"no name":                         {"c", `{"paths":["art"],"min_prob":0.5}`, "display_name"},
		"a name that is too long":         {"c", `{"display_name":"` + long + `","paths":["art"],"min_prob":0.5}`, "display_name"},
		"no topics":                       {"c", `{"display_name":"C","min_prob":0.5}`, "no paths"},
		"a topic that isn't one":          {"c", `{"display_name":"C","paths":["nonsense/topic"],"min_prob":0.5}`, "not a broad topic or subtopic"},
		"a probability of zero":           {"c", `{"display_name":"C","paths":["art"],"min_prob":0}`, "min_prob"},
		"a typo in a setting":             {"c", `{"display_name":"C","paths":["art"],"min_prob":0.5,"minprob":0.7}`, "That isn't a feed"},
		"not JSON":                        {"c", `{"display_name":`, "That isn't a feed"},
		"JSON after the feed":             {"c", catsJSON + ` {"x":1}`, "That isn't a feed"},
		"an array":                        {"c", `[]`, "That isn't a feed"},
		"adult posts":                     {"c", `{"display_name":"C","paths":["art"],"min_prob":0.5,"allow_adult":true}`, "adult"},
		"too many topics":                 {"c", `{"display_name":"C","paths":["art","music","humor","sports"],"min_prob":0.5}`, "at most 3 topics"},
		"too many to leave out":           {"c", `{"display_name":"C","paths":["art"],"min_prob":0.5,"exclude":{"music":0.3,"humor":0.3,"sports":0.3}}`, "at most 2 topics to leave out"},
		"too many posts":                  {"c", `{"display_name":"C","paths":["art"],"min_prob":0.5,"max_posts":101}`, "max_posts is at most 100"},
		"a score that isn't one":          {"c", `{"display_name":"C","paths":["art"],"min_prob":0.5,"tone":{"max":{"furious":0.3}}}`, "unknown tone"},
	} {
		code, out := r.save(t, "did:plc:bob", c.rkey, c.body)
		msg, _ := out["message"].(string)
		if code != 400 || out["error"] != "invalid" || !strings.Contains(msg, c.want) {
			t.Errorf("%s: %d %v (want a 400 saying %q)", name, code, out, c.want)
		}
	}
	if got := r.list(t, "did:plc:bob"); len(got.Feeds) != 0 {
		t.Errorf("a refused feed was stored: %+v", got.Feeds)
	}
}

func TestTheOwnerMayDoWhatOthersMayNot(t *testing.T) {
	r := newFeedsRig(t)
	adult := `{"display_name":"Adult","paths":["adult_content"],"min_prob":0.5,"allow_adult":true,"max_posts":9000}`
	if code, out := r.save(t, "did:plc:owner", "adult", adult); code != 201 {
		t.Errorf("the owner's adult feed: %d %v", code, out)
	}
	many := `{"display_name":"Many","paths":["art","music","humor","sports","gaming"],"min_prob":0.5}`
	for i := 0; i < 5; i++ {
		if code, out := r.save(t, "did:plc:owner", fmt.Sprintf("f%d", i), many); code != 201 {
			t.Errorf("the owner's feed %d: %d %v", i, code, out)
		}
	}
	// The owner's feeds are served as the service's own, with no owner.
	if stored, _ := r.store.OwnerFeeds(context.Background(), "did:plc:owner"); len(stored) != 6 || stored[0].Feed("did:plc:owner").Owner != "" {
		t.Errorf("%d stored", len(stored))
	}
}

func TestThereIsALimitToHowManyFeedsAPersonMakesButNotToHowManyTimesTheyChangeOne(t *testing.T) {
	r := newFeedsRig(t)
	for _, k := range []string{"one", "two"} {
		if code, out := r.save(t, "did:plc:bob", k, catsJSON); code != 201 {
			t.Fatalf("%s: %d %v", k, code, out)
		}
	}
	code, out := r.save(t, "did:plc:bob", "three", catsJSON)
	if code != 409 || out["error"] != "limit" || !strings.Contains(out["message"].(string), "at most 2") {
		t.Errorf("a third: %d %v", code, out)
	}
	if code, _ := r.save(t, "did:plc:bob", "two", strings.Replace(catsJSON, `"Cats"`, `"Two"`, 1)); code != 200 {
		t.Errorf("changing one they have: %d", code)
	}
	// Someone else's feeds don't count against them, and removing one makes room.
	if code, _ := r.save(t, "did:plc:alice", "one", catsJSON); code != 201 {
		t.Errorf("another account: %d", code)
	}
	r.do(t, feedsCall{method: "DELETE", path: "/api/me/feeds/one", viewer: "did:plc:bob"})
	if code, out := r.save(t, "did:plc:bob", "three", catsJSON); code != 201 {
		t.Errorf("after deleting one: %d %v", code, out)
	}
	// And a feed taken away can be made again.
	if code, _ := r.save(t, "did:plc:bob", "one", catsJSON); code != 409 {
		t.Errorf("that is two again plus one: %d", code)
	}
}

func TestChangesMustComeFromThePageAndBeJSON(t *testing.T) {
	r := newFeedsRig(t)
	for name, c := range map[string]struct {
		call feedsCall
		want int
	}{
		"another site's page":     {feedsCall{method: "PUT", path: "/api/me/feeds/cats", viewer: "did:plc:bob", body: catsJSON, header: map[string]string{"Origin": "https://evil.example"}}, 403},
		"a cross-site fetch":      {feedsCall{method: "PUT", path: "/api/me/feeds/cats", viewer: "did:plc:bob", body: catsJSON, header: map[string]string{"Sec-Fetch-Site": "cross-site"}}, 403},
		"a form post":             {feedsCall{method: "PUT", path: "/api/me/feeds/cats", viewer: "did:plc:bob", body: catsJSON, header: map[string]string{"Content-Type": "application/x-www-form-urlencoded"}}, 415},
		"plain text":              {feedsCall{method: "PUT", path: "/api/me/feeds/cats", viewer: "did:plc:bob", body: catsJSON, header: map[string]string{"Content-Type": "text/plain"}}, 415},
		"a delete from elsewhere": {feedsCall{method: "DELETE", path: "/api/me/feeds/cats", viewer: "did:plc:bob", header: map[string]string{"Origin": "https://evil.example"}}, 403},
		"the page itself":         {feedsCall{method: "PUT", path: "/api/me/feeds/cats", viewer: "did:plc:bob", body: catsJSON, header: map[string]string{"Origin": feedsOrigin, "Sec-Fetch-Site": "same-origin"}}, 201},
	} {
		if code, _, _ := r.do(t, c.call); code != c.want {
			t.Errorf("%s: %d, want %d", name, code, c.want)
		}
	}
	if got := r.list(t, "did:plc:bob"); len(got.Feeds) != 1 {
		t.Errorf("only the page's change was made: %+v", got.Feeds)
	}
}

func TestAFeedThatIsFarTooBigIsRefused(t *testing.T) {
	r := newFeedsRig(t)
	body := `{"display_name":"C","paths":["art"],"min_prob":0.5,"description":"` + strings.Repeat("x", 70<<10) + `"}`
	if code, out := r.save(t, "did:plc:bob", "c", body); code != 413 || out["error"] != "too_large" {
		t.Errorf("%d %v", code, out)
	}
}

func TestAnAccountCanNotChangeFeedsFasterThanTheLimit(t *testing.T) {
	r := newFeedsRig(t)
	r.api.Edits = NewIPLimiter(time.Hour, 2)
	codes := []int{}
	for i := 0; i < 4; i++ {
		code, _ := r.save(t, "did:plc:bob", "cats", catsJSON)
		codes = append(codes, code)
	}
	if fmt.Sprint(codes) != "[201 200 429 429]" {
		t.Errorf("%v", codes)
	}
	// Someone else is not slowed by it.
	if code, _ := r.save(t, "did:plc:alice", "cats", catsJSON); code != 201 {
		t.Errorf("another account: %d", code)
	}
}

func TestTroubleWithTheDatabaseIsSaidWithoutItsDetails(t *testing.T) {
	for _, on := range []string{"list", "save", "delete"} {
		r := newFeedsRig(t)
		r.store.failOn = on
		var rec *httptest.ResponseRecorder
		var code int
		switch on {
		case "list":
			code, _, rec = r.do(t, feedsCall{method: "GET", path: "/api/me/feeds", viewer: "did:plc:bob"})
		case "save":
			code, _, rec = r.do(t, feedsCall{method: "PUT", path: "/api/me/feeds/cats", viewer: "did:plc:bob", body: catsJSON})
		case "delete":
			code, _, rec = r.do(t, feedsCall{method: "DELETE", path: "/api/me/feeds/cats", viewer: "did:plc:bob"})
		}
		if code != 503 || strings.Contains(rec.Body.String(), "secret") || strings.Contains(rec.Body.String(), "clickhouse") {
			t.Errorf("%s: %d %s", on, code, rec.Body)
		}
	}
	// A save that failed made no feed and told the service of none.
	r := newFeedsRig(t)
	r.store.failOn = "save"
	r.save(t, "did:plc:bob", "cats", catsJSON)
	if len(r.feeds.syncNow) != 0 {
		t.Error("the service was told of a change that was not made")
	}
}

func TestBrowserClientMetadataIsWhatBlueskysServersNeed(t *testing.T) {
	r := newFeedsRig(t)
	code, meta, rec := r.do(t, feedsCall{method: "GET", path: "/oauth/browser-client-metadata.json"})
	if code != 200 || !strings.HasPrefix(rec.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("%d %s", code, rec.Header().Get("Content-Type"))
	}
	if meta["client_id"] != feedsOrigin+"/oauth/browser-client-metadata.json" {
		t.Errorf("the client ID is the address the document is served from: %v", meta["client_id"])
	}
	scope, _ := meta["scope"].(string)
	if scope != "atproto repo:app.bsky.feed.generator" || strings.Contains(scope, "transition:") || strings.Contains(scope, "blob:") {
		t.Errorf("only granular permissions: %q", scope)
	}
	uris, _ := meta["redirect_uris"].([]any)
	if len(uris) != 1 || uris[0] != feedsOrigin+"/feeds" {
		t.Errorf("redirect_uris: %v", uris)
	}
	if meta["token_endpoint_auth_method"] != "none" || meta["application_type"] != "web" || meta["dpop_bound_access_tokens"] != true {
		t.Errorf("a public client in the browser: %v", meta)
	}
	if g, _ := meta["grant_types"].([]any); fmt.Sprint(g) != "[authorization_code refresh_token]" {
		t.Errorf("grant_types: %v", g)
	}
	if rt, _ := meta["response_types"].([]any); fmt.Sprint(rt) != "[code]" {
		t.Errorf("response_types: %v", rt)
	}
	// It is a client of its own, not the one that signs people in.
	if meta["client_id"] == feedsOrigin+"/oauth/client-metadata.json" {
		t.Error("the signing-in client is not this one")
	}
	if rec.Header().Get("Cache-Control") == "" || rec.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers: %v", rec.Header())
	}
}

func TestHandlesAreResolvedForThePublishingPage(t *testing.T) {
	r := newFeedsRig(t)
	var asked []string
	r.s.ResolveHandle = &ResolveHandleAPI{
		Resolve: func(_ context.Context, h string) (string, error) {
			asked = append(asked, h)
			switch h {
			case "alice.example.com":
				return "did:plc:alice", nil
			case "broken.example.com":
				return "", errors.New("dial tcp: i/o timeout, internal detail")
			}
			return "", ErrNoSuchHandle
		},
		Limit: NewIPLimiter(time.Hour, 6), Log: quietLog, // every question counts, whatever it asks
	}
	get := func(q string) (int, map[string]any) {
		code, out, _ := r.do(t, feedsCall{method: "GET", path: "/xrpc/com.atproto.identity.resolveHandle" + q})
		return code, out
	}
	if code, out := get("?handle=alice.example.com"); code != 200 || out["did"] != "did:plc:alice" {
		t.Errorf("a handle: %d %v", code, out)
	}
	for name, q := range map[string]string{"no such handle": "?handle=nobody.example.com", "a failure": "?handle=broken.example.com", "no handle given": "", "an empty one": "?handle=", "a huge one": "?handle=" + url.QueryEscape(strings.Repeat("a", 300))} {
		code, out := get(q)
		if code != 400 || out["error"] != "InvalidRequest" || strings.Contains(fmt.Sprint(out), "internal detail") {
			t.Errorf("%s: %d %v", name, code, out)
		}
	}
	if len(asked) != 3 {
		t.Errorf("asked of the resolver: %v (a missing or huge handle is never looked up)", asked)
	}
	// The seventh question from one address is too many (the limiter admits six).
	if code, out := get("?handle=alice.example.com"); code != 429 {
		t.Errorf("past the limit: %d %v", code, out)
	}
}

func TestDescribingTheServiceListsSomeOfEveryonesFeedsAndNotAll(t *testing.T) {
	r := newFeedsRig(t)
	nfl := Feed{Rkey: "nfl", DisplayName: "NFL", Paths: []string{"sports/american_football"}, MinProb: 0.5, Ranking: DefaultRanking}
	feeds := []Feed{nfl}
	for i := 0; i < maxDescribedUserFeeds+30; i++ {
		feeds = append(feeds, personFeed(fmt.Sprintf("did:plc:p%03d", i), "cats"))
	}
	r.feeds.Reconcile(feeds)
	_, body, _ := r.do(t, feedsCall{method: "GET", path: "/xrpc/app.bsky.feed.describeFeedGenerator"})
	listed, _ := body["feeds"].([]any)
	if len(listed) != 1+maxDescribedUserFeeds {
		t.Fatalf("%d listed", len(listed))
	}
	if listed[0].(map[string]any)["uri"] != "at://did:plc:owner/app.bsky.feed.generator/nfl" || listed[1].(map[string]any)["uri"] != "at://did:plc:p000/app.bsky.feed.generator/cats" {
		t.Errorf("the owner's first, then the oldest of the others: %v %v", listed[0], listed[1])
	}
}
