package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/jcalabro/atmos"
	atmosidentity "github.com/jcalabro/atmos/identity"

	"github.com/haileyok/topic-feed/internal/feedgen"
	"github.com/haileyok/topic-feed/internal/taxonomy"
)

const postURI = "at://did:plc:alicealicealicealicealic/app.bsky.feed.post/3kabc"

// remote serves what Bluesky's public API answers to getPosts, and says what it was asked.
func remote(t *testing.T, status int, body string, asked *http.Request) *appViewPosts {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if asked != nil {
			*asked = *r
		}
		w.WriteHeader(status)
		fmt.Fprint(w, body)
	}))
	t.Cleanup(srv.Close)
	return &appViewPosts{BaseURL: srv.URL, Client: srv.Client()}
}

func TestRemoteLookupSaysWhatKindOfPostItIs(t *testing.T) {
	post := func(record string) string {
		return fmt.Sprintf(`{"posts":[{"uri":%q,"cid":"bafy","record":%s}]}`, postURI, record)
	}
	cases := []struct {
		name string
		body string
		want feedgen.RemotePost
	}{
		{"a top-level post in English", post(`{"text":"hi","langs":["en"],"createdAt":"2026-10-04T12:00:00Z"}`),
			feedgen.RemotePost{Found: true, Langs: []string{"en"}, CreatedAt: time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)}},
		{"a reply", post(`{"text":"hi","langs":["en"],"reply":{"root":{"uri":"x"},"parent":{"uri":"x"}}}`), feedgen.RemotePost{Found: true, IsReply: true, Langs: []string{"en"}}},
		{"a reply that is null is not one", post(`{"reply":null,"langs":["en"]}`), feedgen.RemotePost{Found: true, Langs: []string{"en"}}},
		{"a post in another language", post(`{"langs":["ja","en"]}`), feedgen.RemotePost{Found: true, Langs: []string{"ja", "en"}}},
		{"no language tag, and a date that isn't one", post(`{"createdAt":"yesterday"}`), feedgen.RemotePost{Found: true}},
		{"a post Bluesky doesn't return", `{"posts":[]}`, feedgen.RemotePost{}},
		{"only some other post", `{"posts":[{"uri":"at://did:plc:other/app.bsky.feed.post/1","record":{"langs":["en"]}}]}`, feedgen.RemotePost{}},
		{"a post with no record at all", fmt.Sprintf(`{"posts":[{"uri":%q}]}`, postURI), feedgen.RemotePost{Found: true}},
	}
	for _, c := range cases {
		got, err := remote(t, 200, c.body, nil).Lookup(context.Background(), postURI)
		if err != nil {
			t.Errorf("%s: %v", c.name, err)
			continue
		}
		if fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%s: %+v, want %+v", c.name, got, c.want)
		}
	}
}

func TestRemoteLookupAsksForJustThePost(t *testing.T) {
	var asked http.Request
	if _, err := remote(t, 200, `{"posts":[]}`, &asked).Lookup(context.Background(), postURI); err != nil {
		t.Fatal(err)
	}
	if asked.Method != http.MethodGet || asked.URL.Path != "/xrpc/app.bsky.feed.getPosts" {
		t.Errorf("%s %s", asked.Method, asked.URL)
	}
	if got := asked.URL.Query()["uris"]; len(got) != 1 || got[0] != postURI {
		t.Errorf("uris = %v", got)
	}
	if asked.Header.Get("User-Agent") == "" || asked.Header.Get("Accept") != "application/json" {
		t.Errorf("headers: %v", asked.Header)
	}
	if asked.Header.Get("Authorization") != "" || asked.Header.Get("Cookie") != "" {
		t.Errorf("nothing of the owner's goes to Bluesky: %v", asked.Header)
	}
}

func TestRemoteLookupFailuresAreErrorsNotAnAnswer(t *testing.T) {
	huge := `{"posts":[],"pad":"` + strings.Repeat("x", maxRemoteBody+10) + `"}`
	for name, a := range map[string]*appViewPosts{
		"a server error":        remote(t, 500, `{"error":"boom"}`, nil),
		"a rate limit":          remote(t, 429, `{}`, nil),
		"an invalid request":    remote(t, 400, `{"error":"InvalidRequest"}`, nil),
		"an answer that isn't":  remote(t, 200, `not json`, nil),
		"an empty answer":       remote(t, 200, ``, nil),
		"an answer far too big": remote(t, 200, huge, nil),
		"no server at all":      {BaseURL: "http://127.0.0.1:1", Client: &http.Client{Timeout: time.Second}},
	} {
		got, err := a.Lookup(context.Background(), postURI)
		if err == nil {
			t.Errorf("%s: %+v, want an error: Bluesky not answering must not read as 'it doesn't exist'", name, got)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := remote(t, 200, `{"posts":[]}`, nil).Lookup(ctx, postURI); err == nil {
		t.Error("a cancelled lookup is an error")
	}
}

// directory is an identity directory over made-up accounts.
type notFoundResolver struct {
	handles map[string]atmos.DID
	docs    map[string]*atmosidentity.DIDDocument
	fail    error // what every lookup of a handle answers, if set
	asked   []string
}

func (r *notFoundResolver) ResolveDID(_ context.Context, did atmos.DID) (*atmosidentity.DIDDocument, error) {
	if d, ok := r.docs[string(did)]; ok {
		return d, nil
	}
	return nil, atmosidentity.ErrDIDNotFound
}

func (r *notFoundResolver) ResolveHandle(_ context.Context, h atmos.Handle) (atmos.DID, error) {
	r.asked = append(r.asked, string(h))
	if r.fail != nil {
		return "", r.fail
	}
	if d, ok := r.handles[string(h)]; ok {
		return d, nil
	}
	// This is how the real resolver says a handle isn't known to anybody.
	return "", fmt.Errorf("%w: https://%s/.well-known/atproto-did: 404", atmosidentity.ErrHandleNotFound, h)
}

func TestInspectorFindsTheAccountOfAHandle(t *testing.T) {
	doc := func(did, handle string) *atmosidentity.DIDDocument {
		return &atmosidentity.DIDDocument{ID: did, AlsoKnownAs: []string{"at://" + handle}, Service: []atmosidentity.Service{
			{ID: "#atproto_pds", Type: "AtprotoPersonalDataServer", ServiceEndpoint: "https://pds.example.test"}}}
	}
	const alice, mallory = "did:plc:alicealicealicealicealic", "did:plc:mallorymallorymallorymal"
	res := &notFoundResolver{
		handles: map[string]atmos.DID{"alice.example.test": alice, "claims-to-be-mallory.example.test": mallory},
		// mallory's account names another handle: a handle that points at her doesn't check out
		docs: map[string]*atmosidentity.DIDDocument{alice: doc(alice, "alice.example.test"), mallory: doc(mallory, "mallory.example.test")},
	}
	resolve := didOfHandle(&atmosidentity.Directory{Resolver: res})
	ctx := context.Background()

	if did, err := resolve(ctx, "alice.example.test"); err != nil || did != alice {
		t.Errorf("alice: %q, %v", did, err)
	}
	if did, err := resolve(ctx, "Alice.Example.Test"); err != nil || did != alice {
		t.Errorf("a handle in capitals is the same handle: %q, %v", did, err)
	}
	for _, handle := range []string{"nobody.example.test", "claims-to-be-mallory.example.test"} {
		if did, err := resolve(ctx, handle); !errors.Is(err, feedgen.ErrNoSuchHandle) || did != "" {
			t.Errorf("%q: %q, %v, want ErrNoSuchHandle", handle, did, err)
		}
	}
	// What isn't a handle is never looked up: the lookup would go to whatever host the text names.
	asked := len(res.asked)
	for _, notAHandle := range []string{"not a handle", "", "x", "127.0.0.1", "a.example.test/../b", "evil.example.test:8080", "user@example.test", "-a.example.test"} {
		if did, err := resolve(ctx, notAHandle); !errors.Is(err, feedgen.ErrNoSuchHandle) || did != "" {
			t.Errorf("%q: %q, %v, want ErrNoSuchHandle", notAHandle, did, err)
		}
	}
	if len(res.asked) != asked {
		t.Errorf("looked up %v, which are not handles", res.asked[asked:])
	}

	// Not reaching anyone is not the same as there being no such account.
	down := didOfHandle(&atmosidentity.Directory{Resolver: &notFoundResolver{fail: errors.New("dial tcp: i/o timeout")}})
	if did, err := down(ctx, "alice.example.test"); err == nil || errors.Is(err, feedgen.ErrNoSuchHandle) || did != "" {
		t.Errorf("a failure to look: %q, %v", did, err)
	}
	// The real resolver words a failed request as 'not found' too: when we gave up waiting, it is not.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	slow := didOfHandle(&atmosidentity.Directory{Resolver: &notFoundResolver{fail: fmt.Errorf("%w: context canceled", atmosidentity.ErrHandleNotFound)}})
	if did, err := slow(cancelled, "alice.example.test"); err == nil || errors.Is(err, feedgen.ErrNoSuchHandle) || did != "" {
		t.Errorf("a lookup that was cut short: %q, %v", did, err)
	}
}

func TestInspectorIsWiredToTheOwnerAndAnswersNobodyElse(t *testing.T) {
	t.Setenv("FEEDGEN_SESSION_SECRET", strings.Repeat("s", 32))
	signIn, handle, err := newSignIn("https://feeds.example.test", quiet)
	if err != nil || signIn == nil {
		t.Fatalf("%v, %v", signIn, err)
	}
	api := newInspectAPI("did:plc:alicealicealicealicealic", signIn, &feedgen.Store{}, nil, nil, &taxonomy.Taxonomy{}, handle, quiet)
	if api.Owner != "did:plc:alicealicealicealicealic" || api.Viewer == nil || api.Source == nil || api.Resolve == nil || api.Remote == nil || api.Limit == nil || api.Handle == nil {
		t.Fatalf("not fully set up: %+v", api)
	}
	// Nobody is signed in on this request: the answer is 401 before anything is read.
	srv := httptest.NewServer(http.HandlerFunc(api.ServeInspect))
	defer srv.Close()
	for _, q := range []string{"", "?post=" + postURI} {
		resp, err := http.Get(srv.URL + "/api/inspect" + q)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusUnauthorized {
			t.Errorf("%q: %d, want 401", q, resp.StatusCode)
		}
	}
}
