package imagearchive

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// newAppViewTestServer answers getPosts with fn, counting calls.
func newAppViewTestServer(t *testing.T, fn func(w http.ResponseWriter, r *http.Request) (int, any)) (*AppView, *int32) {
	t.Helper()
	var calls int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt32(&calls, 1)
		code, body := fn(w, r)
		w.Header().Set("Content-Type", "application/json")
		if code == http.StatusTooManyRequests {
			w.Header().Set("Retry-After", "0")
		}
		w.WriteHeader(code)
		_ = json.NewEncoder(w).Encode(body)
	}))
	t.Cleanup(srv.Close)
	av := &AppView{
		BaseURL: srv.URL,
		Backoff: time.Millisecond, // keep tests fast
	}
	return av, &calls
}

func TestGetPostsOK(t *testing.T) {
	av, calls := newAppViewTestServer(t, func(w http.ResponseWriter, r *http.Request) (int, any) {
		uris := r.URL.Query()["uris"]
		posts := make([]map[string]any, 0, len(uris))
		for _, u := range uris {
			posts = append(posts, map[string]any{"uri": u, "author": map[string]any{"did": "did:plc:a"}})
		}
		return http.StatusOK, map[string]any{"posts": posts}
	})
	ctx := context.Background()
	views, err := av.GetPosts(ctx, []string{"at://did:plc:a/app.bsky.feed.post/1", "at://did:plc:a/app.bsky.feed.post/2"})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 2 || views["at://did:plc:a/app.bsky.feed.post/1"].Author.DID != "did:plc:a" {
		t.Fatalf("views = %+v", views)
	}
	if n := atomic.LoadInt32(calls); n != 1 {
		t.Fatalf("calls = %d, want 1", n)
	}
}

func TestGetPosts429ThenOK(t *testing.T) {
	var served int32
	av, calls := newAppViewTestServer(t, func(w http.ResponseWriter, r *http.Request) (int, any) {
		if atomic.AddInt32(&served, 1) == 1 {
			return http.StatusTooManyRequests, map[string]any{"error": "RateLimitExceeded"}
		}
		return http.StatusOK, map[string]any{"posts": []any{map[string]any{"uri": "at://x/1"}}}
	})
	views, err := av.GetPosts(context.Background(), []string{"at://x/1"})
	if err != nil {
		t.Fatal(err)
	}
	if len(views) != 1 {
		t.Fatalf("views = %+v", views)
	}
	if n := atomic.LoadInt32(calls); n != 2 {
		t.Fatalf("calls = %d, want 2 (one rate limited, one ok)", n)
	}
}

func TestGetPosts400(t *testing.T) {
	av, _ := newAppViewTestServer(t, func(w http.ResponseWriter, r *http.Request) (int, any) {
		return http.StatusBadRequest, map[string]any{"error": "InvalidRequest"}
	})
	_, err := av.GetPosts(context.Background(), []string{"at://x/1"})
	var se *StatusError
	if !errors.As(err, &se) || se.Code != 400 {
		t.Fatalf("err = %v, want *StatusError 400", err)
	}
}

func TestGetPostsMissingURIAbsent(t *testing.T) {
	av, _ := newAppViewTestServer(t, func(w http.ResponseWriter, r *http.Request) (int, any) {
		return http.StatusOK, map[string]any{"posts": []any{map[string]any{"uri": "at://x/1"}}}
	})
	views, err := av.GetPosts(context.Background(), []string{"at://x/1", "at://x/deleted"})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := views["at://x/deleted"]; ok {
		t.Fatal("deleted post came back; it must be absent")
	}
}

func TestGetPostsTooManyURIs(t *testing.T) {
	av, _ := newAppViewTestServer(t, func(w http.ResponseWriter, r *http.Request) (int, any) {
		return http.StatusOK, map[string]any{"posts": []any{}}
	})
	uris := make([]string, MaxURIsPerCall+1)
	for i := range uris {
		uris[i] = fmt.Sprintf("at://x/%d", i)
	}
	if _, err := av.GetPosts(context.Background(), uris); err == nil {
		t.Fatal("26 uris must be an error")
	}
}
