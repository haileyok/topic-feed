package feedgen

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/haileyok/topic-feed/internal/signin"
)

const signInOrigin = "https://feeds.example.com"

type fakeSignInAuth struct{}

func (fakeSignInAuth) Start(context.Context, string) (string, string, error) {
	return "https://pds.example.test/oauth/authorize?request_uri=urn%3Aabc", "state-1", nil
}

func (fakeSignInAuth) Finish(context.Context, string, string, string) (string, error) {
	return "did:plc:ragtjsm2j2vknwkz3zp4oxrd", nil
}

func serverWithSignIn(t *testing.T) *Server {
	t.Helper()
	s := testServer(t)
	sessions, err := signin.NewSessions([]byte(strings.Repeat("k", 32)), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	h, err := signin.New(signin.Config{
		Origin: signInOrigin, Auth: fakeSignInAuth{}, Metadata: signin.ClientMetadataFor(signInOrigin, "Test"), Sessions: sessions,
		Handle: func(context.Context, string) string { return "alice.example.test" },
		Log:    slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	s.SignIn = h
	return s
}

func serve(s *Server, method, target string, body io.Reader, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, target, body)
	if method == "POST" {
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

var signInRoutes = []struct{ method, path string }{
	{"GET", "/oauth/client-metadata.json"},
	{"POST", "/oauth/login"},
	{"GET", "/oauth/callback"},
	{"POST", "/oauth/logout"},
	{"GET", "/api/me"},
}

func TestSignInRoutesAreNotFoundWhileSignInIsOff(t *testing.T) {
	s := testServer(t)
	for _, r := range signInRoutes {
		w := serve(s, r.method, r.path, strings.NewReader("handle=alice.example.test"))
		if w.Code != http.StatusNotFound {
			t.Errorf("%s %s: %d", r.method, r.path, w.Code)
		}
	}
}

func TestSignInRoutesWorkOnTheServer(t *testing.T) {
	s := serverWithSignIn(t)

	w := serve(s, "GET", "/oauth/client-metadata.json", nil)
	var meta map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &meta); err != nil || w.Code != 200 || meta["client_id"] != signInOrigin+"/oauth/client-metadata.json" {
		t.Fatalf("metadata: %d %s", w.Code, w.Body.String())
	}

	w = serve(s, "POST", "/oauth/login", strings.NewReader(url.Values{"handle": {"alice.example.test"}}.Encode()))
	if w.Code != http.StatusSeeOther || !strings.HasPrefix(w.Header().Get("Location"), "https://pds.example.test/oauth/authorize") {
		t.Fatalf("login: %d %q", w.Code, w.Header().Get("Location"))
	}
	var state *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "__Host-feeds_login" {
			state = c
		}
	}
	if state == nil {
		t.Fatalf("no login cookie: %v", w.Result().Cookies())
	}

	w = serve(s, "GET", "/oauth/callback?code=abc&state=state-1&iss=x", nil, state)
	var session *http.Cookie
	for _, c := range w.Result().Cookies() {
		if c.Name == "__Host-feeds_session" {
			session = c
		}
	}
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/me" || session == nil {
		t.Fatalf("callback: %d %q %v", w.Code, w.Header().Get("Location"), w.Result().Cookies())
	}

	w = serve(s, "GET", "/api/me", nil, session)
	if w.Code != 200 || !strings.Contains(w.Body.String(), "did:plc:ragtjsm2j2vknwkz3zp4oxrd") || !strings.Contains(w.Body.String(), "alice.example.test") {
		t.Errorf("/api/me: %d %s", w.Code, w.Body.String())
	}
	if w := serve(s, "GET", "/api/me", nil); w.Code != http.StatusUnauthorized {
		t.Errorf("/api/me signed out: %d", w.Code)
	}

	w = serve(s, "POST", "/oauth/logout", nil, session)
	var cleared bool
	for _, c := range w.Result().Cookies() {
		if c.Name == "__Host-feeds_session" && c.MaxAge < 0 {
			cleared = true
		}
	}
	if w.Code != http.StatusSeeOther || w.Header().Get("Location") != "/me" || !cleared {
		t.Errorf("logout: %d %q, cookies %v", w.Code, w.Header().Get("Location"), w.Result().Cookies())
	}

	// Routes have their own methods, and the rest of the service is unaffected.
	if w := serve(s, "GET", "/oauth/login", nil); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET /oauth/login: %d", w.Code)
	}
	if w := serve(s, "GET", "/healthz", nil); w.Code != 200 {
		t.Errorf("/healthz: %d", w.Code)
	}
}
