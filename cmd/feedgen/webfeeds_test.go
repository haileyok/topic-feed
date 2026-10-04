package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/haileyok/topic-feed/internal/feedgen"
)

func TestPeoplesFeedsAreWiredToTheSignedInAccountOnly(t *testing.T) {
	t.Setenv("FEEDGEN_SESSION_SECRET", strings.Repeat("s", 32))
	signIn, _, err := newSignIn("https://feeds.example.test", quiet)
	if err != nil || signIn == nil {
		t.Fatalf("%v, %v", signIn, err)
	}
	api := newFeedsAPI("did:plc:owner", "did:web:feeds.example.test", "https://feeds.example.test", signIn, &feedgen.Store{}, nil,
		map[string]bool{"art": true}, quiet)
	if api.Owner != "did:plc:owner" || api.ServiceDID != "did:web:feeds.example.test" || api.Origin != "https://feeds.example.test" ||
		api.Viewer == nil || api.Store == nil || api.Edits == nil || api.Paths == nil || api.Limits != feedgen.DefaultUserLimits {
		t.Fatalf("not fully set up: %+v", api)
	}
	// Nobody is signed in on these requests: 401 before anything is read.
	for _, c := range []struct{ method, path string }{{"GET", "/api/me/feeds"}, {"PUT", "/api/me/feeds/cats"}, {"DELETE", "/api/me/feeds/cats"}} {
		rec := httptest.NewRecorder()
		req := httptest.NewRequest(c.method, c.path, strings.NewReader(`{}`))
		req.SetPathValue("rkey", "cats")
		switch c.method {
		case "GET":
			api.ServeList(rec, req)
		case "PUT":
			api.ServeSave(rec, req)
		default:
			api.ServeDelete(rec, req)
		}
		if rec.Code != http.StatusUnauthorized {
			t.Errorf("%s %s: %d, want 401", c.method, c.path, rec.Code)
		}
	}
}

func TestHandleLookupsForThePublishingPageAreLimited(t *testing.T) {
	api := newResolveHandleAPI(quiet)
	if api.Resolve == nil || api.Limit == nil {
		t.Fatalf("%+v", api)
	}
	// A string that isn't a handle is refused without a lookup (didOfHandle never sends it anywhere).
	rec := httptest.NewRecorder()
	api.ServeResolve(rec, httptest.NewRequest("GET", "/xrpc/com.atproto.identity.resolveHandle?handle=127.0.0.1", nil))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "InvalidRequest") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}
