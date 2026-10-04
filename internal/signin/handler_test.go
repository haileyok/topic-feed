package signin

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
	"sync"
	"testing"
	"time"
)

const testOrigin = "https://feeds.example.test"

type finishCall struct{ code, state, iss string }

// fakeAuth is an Authenticator that answers as told and remembers what it was asked.
type fakeAuth struct {
	mu          sync.Mutex
	startErr    error
	finishErr   error
	redirect    string
	state       string
	did         string
	accounts    []string
	finishCalls []finishCall
}

func newFakeAuth() *fakeAuth {
	return &fakeAuth{redirect: "https://pds.example.test/oauth/authorize?request_uri=urn%3Aabc", state: "state-123", did: testDID}
}

func (f *fakeAuth) Start(_ context.Context, account string) (string, string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.accounts = append(f.accounts, account)
	if f.startErr != nil {
		return "", "", f.startErr
	}
	return f.redirect, f.state, nil
}

func (f *fakeAuth) Finish(_ context.Context, code, state, iss string) (string, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.finishCalls = append(f.finishCalls, finishCall{code, state, iss})
	if f.finishErr != nil {
		return "", f.finishErr
	}
	return f.did, nil
}

func (f *fakeAuth) started() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.accounts...)
}

func (f *fakeAuth) finished() []finishCall {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]finishCall(nil), f.finishCalls...)
}

type rig struct {
	t    *testing.T
	h    *Handler
	mux  *http.ServeMux
	auth *fakeAuth
	sess *Sessions
	clk  *clock
}

func newRig(t *testing.T, tweak func(*Config)) *rig {
	t.Helper()
	sess, clk := newSessions(t)
	auth := newFakeAuth()
	cfg := Config{
		Origin: testOrigin, Auth: auth, Sessions: sess,
		Metadata: ClientMetadataFor(testOrigin, "Test feeds"),
		Handle: func(_ context.Context, did string) string {
			if did == testDID {
				return "alice.example.test"
			}
			return ""
		},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	if tweak != nil {
		tweak(&cfg)
	}
	h, err := New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	mux := http.NewServeMux()
	h.Mount(mux)
	return &rig{t: t, h: h, mux: mux, auth: auth, sess: sess, clk: clk}
}

type req struct {
	method, target string
	form           url.Values
	header         map[string]string
	cookies        []*http.Cookie
}

func (r *rig) do(q req) *httptest.ResponseRecorder {
	r.t.Helper()
	var body io.Reader
	if q.form != nil {
		body = strings.NewReader(q.form.Encode())
	}
	hr := httptest.NewRequest(q.method, q.target, body)
	if q.form != nil {
		hr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	for k, v := range q.header {
		hr.Header.Set(k, v)
	}
	for _, c := range q.cookies {
		hr.AddCookie(c)
	}
	w := httptest.NewRecorder()
	r.mux.ServeHTTP(w, hr)
	return w
}

func cookieNamed(w *httptest.ResponseRecorder, name string) *http.Cookie {
	for _, c := range w.Result().Cookies() {
		if c.Name == name {
			return c
		}
	}
	return nil
}

func location(w *httptest.ResponseRecorder) string { return w.Header().Get("Location") }

func login(r *rig, account string) *httptest.ResponseRecorder {
	return r.do(req{method: "POST", target: "/oauth/login", form: url.Values{"handle": {account}}})
}

const stateCookieName, sessionCookieName = "__Host-feeds_login", "__Host-feeds_session"

func TestMetadataDocument(t *testing.T) {
	r := newRig(t, nil)
	w := r.do(req{method: "GET", target: "/oauth/client-metadata.json"})
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
		t.Fatalf("%d %s", w.Code, w.Header().Get("Content-Type"))
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["client_id"] != testOrigin+"/oauth/client-metadata.json" {
		t.Errorf("client_id %v: it must be the address this document is served from", m["client_id"])
	}
	if uris, _ := m["redirect_uris"].([]any); len(uris) != 1 || uris[0] != testOrigin+"/oauth/callback" {
		t.Errorf("redirect_uris %v", m["redirect_uris"])
	}
	if m["token_endpoint_auth_method"] != "none" || m["scope"] != "atproto" || m["dpop_bound_access_tokens"] != true ||
		m["application_type"] != "web" || m["client_name"] != "Test feeds" {
		t.Errorf("%v", m)
	}
	if m["jwks"] != nil || m["jwks_uri"] != nil {
		t.Error("a public client has no keys to publish")
	}
	if w.Header().Get("Cache-Control") == "" || w.Header().Get("X-Content-Type-Options") != "nosniff" {
		t.Errorf("headers %v", w.Header())
	}
	if w := r.do(req{method: "POST", target: "/oauth/client-metadata.json"}); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", w.Code)
	}
}

func TestLoginStartsAndTiesTheLoginToThisBrowser(t *testing.T) {
	r := newRig(t, nil)
	w := login(r, "alice.example.test")
	if w.Code != http.StatusSeeOther || location(w) != r.auth.redirect {
		t.Fatalf("%d, %q", w.Code, location(w))
	}
	c := cookieNamed(w, stateCookieName)
	if c == nil {
		t.Fatalf("no cookie for the login's state: %v", w.Result().Cookies())
	}
	if c.Value != "state-123" || !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" ||
		c.MaxAge != int(pendingTTL.Seconds()) || c.Domain != "" {
		t.Errorf("state cookie %+v", c)
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Errorf("Cache-Control %q", w.Header().Get("Cache-Control"))
	}
	if got := r.auth.started(); len(got) != 1 || got[0] != "alice.example.test" {
		t.Errorf("started %v", got)
	}
}

func TestLoginTidiesWhatWasTyped(t *testing.T) {
	for in, want := range map[string]string{
		"alice.example.test":                                  "alice.example.test",
		"  Alice.Example.Test \n":                             "alice.example.test",
		"@alice.example.test":                                 "alice.example.test",
		"@@alice.example.test":                                "@alice.example.test",
		"https://bsky.app/profile/alice.example.test":         "alice.example.test",
		"https://bsky.app/profile/alice.example.test/":        "alice.example.test",
		"https://bsky.app/profile/alice.example.test/post/3k": "alice.example.test",
		testDID: testDID,
		"https://evil.test/profile/alice.example.test": "https://evil.test/profile/alice.example.test",
	} {
		r := newRig(t, nil)
		login(r, in)
		if got := r.auth.started(); len(got) != 1 || got[0] != want {
			t.Errorf("%q: started %v, want %q", in, got, want)
		}
	}
}

func TestLoginProblemsAreReportedToTheHomePage(t *testing.T) {
	for _, tc := range []struct {
		name    string
		account string
		start   error
		redir   string
		want    string
	}{
		{"nothing typed", "", nil, "", "invalid"},
		{"only space", "   ", nil, "", "invalid"},
		{"far too long", strings.Repeat("a", maxAccountLen+1), nil, "", "invalid"},
		{"not a handle", "not a handle", ErrBadAccount, "", "invalid"},
		{"too many logins", "alice.example.test", ErrBusy, "", "busy"},
		{"the server could not be reached", "alice.example.test", errors.New("dial tcp: no route"), "", "failed"},
		{"a redirect that isn't a web address", "alice.example.test", nil, "javascript:alert(1)", "failed"},
		{"a redirect with no host", "alice.example.test", nil, "https:///oauth", "failed"},
		{"a redirect that is a data URL", "alice.example.test", nil, "data:text/html,hi", "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, nil)
			r.auth.startErr = tc.start
			if tc.redir != "" {
				r.auth.redirect = tc.redir
			}
			w := login(r, tc.account)
			if w.Code != http.StatusSeeOther || location(w) != "/me?signin="+tc.want {
				t.Errorf("%d %q, want a redirect to /me?signin=%s", w.Code, location(w), tc.want)
			}
			if cookieNamed(w, stateCookieName) != nil {
				t.Error("a login that didn't start must not leave a state cookie")
			}
		})
	}
}

func TestLoginRefusesOtherSitesAndRateLimits(t *testing.T) {
	r := newRig(t, nil)
	form := url.Values{"handle": {"alice.example.test"}}
	for name, h := range map[string]map[string]string{
		"another origin":     {"Origin": "https://evil.test"},
		"a lookalike origin": {"Origin": testOrigin + ".evil.test"},
		"http, not https":    {"Origin": "http://feeds.example.test"},
		"the null origin":    {"Origin": "null"},
		"a cross-site fetch": {"Sec-Fetch-Site": "cross-site"},
		"a same-site fetch":  {"Sec-Fetch-Site": "same-site"}, // a sibling subdomain isn't us
	} {
		w := r.do(req{method: "POST", target: "/oauth/login", form: form, header: h})
		if w.Code != http.StatusForbidden {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	if got := r.auth.started(); len(got) != 0 {
		t.Errorf("a request from another site started a login: %v", got)
	}
	for name, h := range map[string]map[string]string{
		"our own origin":       {"Origin": testOrigin},
		"same-origin":          {"Sec-Fetch-Site": "same-origin"},
		"typed in the address": {"Sec-Fetch-Site": "none"},
		"a tool":               nil,
	} {
		if w := r.do(req{method: "POST", target: "/oauth/login", form: form, header: h}); w.Code != http.StatusSeeOther {
			t.Errorf("%s: %d", name, w.Code)
		}
	}
	if w := r.do(req{method: "GET", target: "/oauth/login"}); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", w.Code)
	}

	var allowed bool
	r = newRig(t, func(c *Config) { c.Allow = func(*http.Request) bool { return allowed } })
	if w := login(r, "alice.example.test"); w.Code != http.StatusTooManyRequests || len(r.auth.started()) != 0 {
		t.Errorf("limited: %d, started %v", w.Code, r.auth.started())
	}
	allowed = true
	if w := login(r, "alice.example.test"); w.Code != http.StatusSeeOther {
		t.Errorf("allowed: %d", w.Code)
	}
}

func TestLoginRefusesHugeForms(t *testing.T) {
	r := newRig(t, nil)
	w := login(r, strings.Repeat("a", maxFormBytes*2))
	if w.Code != http.StatusSeeOther || location(w) != "/me?signin=invalid" || len(r.auth.started()) != 0 {
		t.Errorf("%d %q, started %v", w.Code, location(w), r.auth.started())
	}
}

var asJSON = map[string]string{"Accept": "application/json"}

func loginJSON(r *rig, account string) *httptest.ResponseRecorder {
	return r.do(req{method: "POST", target: "/oauth/login", form: url.Values{"handle": {account}}, header: asJSON})
}

func jsonBody(t *testing.T, w *httptest.ResponseRecorder) map[string]string {
	t.Helper()
	var m map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatalf("not JSON: %q (%v)", w.Body.String(), err)
	}
	return m
}

// The page's script can't follow a redirect to another site, so it asks for JSON and is told
// where to go.
func TestLoginAsJSONTellsTheScriptWhereToGo(t *testing.T) {
	r := newRig(t, nil)
	w := loginJSON(r, "alice.example.test")
	if w.Code != 200 || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") || location(w) != "" {
		t.Fatalf("%d %s, Location %q", w.Code, w.Header().Get("Content-Type"), location(w))
	}
	if got := jsonBody(t, w); got["redirect"] != r.auth.redirect || len(got) != 1 {
		t.Errorf("%v", got)
	}
	if c := cookieNamed(w, stateCookieName); c == nil || c.Value != "state-123" || !c.HttpOnly || !c.Secure {
		t.Errorf("the login must still be tied to this browser: %+v", c)
	}
	// A browser keeps the last cookie of a name it is sent, so a second one would undo the first.
	n := 0
	for _, c := range w.Result().Cookies() {
		if c.Name == stateCookieName {
			n++
		}
	}
	if n != 1 {
		t.Errorf("%d cookies set for the login's state, want exactly one", n)
	}
	if got := r.auth.started(); len(got) != 1 || got[0] != "alice.example.test" {
		t.Errorf("started %v", got)
	}
}

func TestLoginProblemsAsJSON(t *testing.T) {
	for _, tc := range []struct {
		name    string
		account string
		start   error
		redir   string
		allow   bool
		status  int
		code    string
	}{
		{"nothing typed", "", nil, "", true, 400, "invalid"},
		{"not a handle", "x", ErrBadAccount, "", true, 400, "invalid"},
		{"too many logins", "alice.example.test", ErrBusy, "", true, 503, "busy"},
		{"the server could not be reached", "alice.example.test", errors.New("no route"), "", true, 502, "failed"},
		{"a redirect that isn't a web address", "alice.example.test", nil, "javascript:alert(1)", true, 502, "failed"},
		{"rate limited", "alice.example.test", nil, "", false, 429, "limited"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, func(c *Config) { c.Allow = func(*http.Request) bool { return tc.allow } })
			r.auth.startErr = tc.start
			if tc.redir != "" {
				r.auth.redirect = tc.redir
			}
			w := loginJSON(r, tc.account)
			if w.Code != tc.status || location(w) != "" || jsonBody(t, w)["error"] != tc.code {
				t.Errorf("%d %q Location %q, want %d %q with no redirect", w.Code, w.Body.String(), location(w), tc.status, tc.code)
			}
			if cookieNamed(w, stateCookieName) != nil {
				t.Error("a login that didn't start must not leave a state cookie")
			}
		})
	}
}

func TestOnlyAScriptThatAsksForJSONGetsJSON(t *testing.T) {
	for name, accept := range map[string]string{
		"a browser navigating": "text/html,application/xhtml+xml,application/xml;q=0.9,image/avif,*/*;q=0.8",
		"anything":             "*/*",
		"nothing":              "",
		"plain text":           "text/plain",
	} {
		r := newRig(t, nil)
		w := r.do(req{method: "POST", target: "/oauth/login", form: url.Values{"handle": {"alice.example.test"}}, header: map[string]string{"Accept": accept}})
		if w.Code != http.StatusSeeOther || location(w) != r.auth.redirect {
			t.Errorf("%s: %d %q", name, w.Code, location(w))
		}
	}
	// A script on another site still can't start a login, however it asks.
	r := newRig(t, nil)
	w := r.do(req{method: "POST", target: "/oauth/login", form: url.Values{"handle": {"alice.example.test"}},
		header: map[string]string{"Accept": "application/json", "Origin": "https://evil.test"}})
	if w.Code != http.StatusForbidden || len(r.auth.started()) != 0 {
		t.Errorf("cross-site: %d, started %v", w.Code, r.auth.started())
	}
}

func TestLogoutAsJSON(t *testing.T) {
	r := newRig(t, nil)
	session := &http.Cookie{Name: sessionCookieName, Value: r.sess.Issue(testDID)}
	w := r.do(req{method: "POST", target: "/oauth/logout", header: asJSON, cookies: []*http.Cookie{session}})
	if w.Code != http.StatusNoContent || location(w) != "" || w.Body.Len() != 0 {
		t.Fatalf("%d, Location %q, body %q", w.Code, location(w), w.Body.String())
	}
	if c := cookieNamed(w, sessionCookieName); c == nil || c.MaxAge >= 0 {
		t.Errorf("cookie %+v", c)
	}
	w = r.do(req{method: "POST", target: "/oauth/logout", header: map[string]string{"Accept": "application/json", "Origin": "https://evil.test"}, cookies: []*http.Cookie{session}})
	if w.Code != http.StatusForbidden || cookieNamed(w, sessionCookieName) != nil {
		t.Errorf("cross-site: %d", w.Code)
	}
}

// finishLogin is the browser coming back from the authorization server.
func finishLogin(r *rig, query string, cookies ...*http.Cookie) *httptest.ResponseRecorder {
	return r.do(req{method: "GET", target: "/oauth/callback?" + query, cookies: cookies})
}

func stateCookie(value string) *http.Cookie {
	return &http.Cookie{Name: stateCookieName, Value: value}
}

func TestCallbackSignsTheBrowserIn(t *testing.T) {
	r := newRig(t, nil)
	w := finishLogin(r, "code=abc&state=state-123&iss=https%3A%2F%2Fpds.example.test", stateCookie("state-123"))
	if w.Code != http.StatusSeeOther || location(w) != "/me" {
		t.Fatalf("%d %q", w.Code, location(w))
	}
	if got := r.auth.finished(); len(got) != 1 || got[0] != (finishCall{"abc", "state-123", "https://pds.example.test"}) {
		t.Errorf("finished with %v", got)
	}
	c := cookieNamed(w, sessionCookieName)
	if c == nil {
		t.Fatalf("no session cookie: %v", w.Result().Cookies())
	}
	if !c.HttpOnly || !c.Secure || c.SameSite != http.SameSiteLaxMode || c.Path != "/" || c.Domain != "" ||
		c.MaxAge != int((24*time.Hour).Seconds()) {
		t.Errorf("session cookie %+v", c)
	}
	if did, ok := r.sess.Verify(c.Value); !ok || did != testDID {
		t.Errorf("the cookie says %q, %v", did, ok)
	}
	if gone := cookieNamed(w, stateCookieName); gone == nil || gone.MaxAge >= 0 {
		t.Errorf("the state cookie should be cleared: %+v", gone)
	}

	// And now /api/me knows them.
	me := r.do(req{method: "GET", target: "/api/me", cookies: []*http.Cookie{c}})
	var got map[string]string
	if err := json.Unmarshal(me.Body.Bytes(), &got); err != nil || me.Code != 200 || got["did"] != testDID || got["handle"] != "alice.example.test" {
		t.Errorf("/api/me: %d %s", me.Code, me.Body.String())
	}
}

func TestCallbackFromALoginThisBrowserDidNotStartIsRefused(t *testing.T) {
	// Somebody sends a victim a link that finishes the attacker's own login: it would sign
	// the victim in as the attacker. The victim's browser never started it, so it has no
	// matching state cookie.
	for name, cookies := range map[string][]*http.Cookie{
		"no cookie":                    nil,
		"another login's cookie":       {stateCookie("state-of-another-login")},
		"an empty cookie":              {stateCookie("")},
		"a cookie that is a prefix":    {stateCookie("state-12")},
		"the cookie in different case": {stateCookie("STATE-123")},
	} {
		t.Run(name, func(t *testing.T) {
			r := newRig(t, nil)
			w := finishLogin(r, "code=abc&state=state-123&iss=x", cookies...)
			if location(w) != "/me?signin=failed" {
				t.Errorf("%d %q", w.Code, location(w))
			}
			if got := r.auth.finished(); len(got) != 0 {
				t.Errorf("a login this browser didn't start was finished: %v", got)
			}
			if cookieNamed(w, sessionCookieName) != nil {
				t.Error("signed in")
			}
		})
	}
	// An empty state with an empty cookie must not count as matching.
	r := newRig(t, nil)
	if w := finishLogin(r, "code=abc&state=", stateCookie("")); location(w) != "/me?signin=failed" || len(r.auth.finished()) != 0 {
		t.Errorf("empty state: %q, finished %v", location(w), r.auth.finished())
	}
}

func TestCallbackProblems(t *testing.T) {
	for _, tc := range []struct {
		name   string
		query  string
		finish error
		want   string
	}{
		{"they said no", "error=access_denied&state=state-123&iss=x", nil, "denied"},
		{"their server said no", "error=server_error&error_description=oops&state=state-123", nil, "denied"},
		{"no code", "state=state-123&iss=x", nil, "failed"},
		{"the code didn't work", "code=abc&state=state-123&iss=x", errors.New("oauth: invalid_grant"), "failed"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			r := newRig(t, nil)
			r.auth.finishErr = tc.finish
			w := finishLogin(r, tc.query, stateCookie("state-123"))
			if location(w) != "/me?signin="+tc.want {
				t.Errorf("%d %q", w.Code, location(w))
			}
			if cookieNamed(w, sessionCookieName) != nil {
				t.Error("signed in")
			}
			if gone := cookieNamed(w, stateCookieName); gone == nil || gone.MaxAge >= 0 {
				t.Error("the state cookie should be cleared whatever happened")
			}
		})
	}
	// A denied login never reaches the token exchange.
	r := newRig(t, nil)
	finishLogin(r, "error=access_denied&state=state-123", stateCookie("state-123"))
	if got := r.auth.finished(); len(got) != 0 {
		t.Errorf("finished %v", got)
	}
	if w := r.do(req{method: "POST", target: "/oauth/callback"}); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("POST: %d", w.Code)
	}
}

func TestLogout(t *testing.T) {
	r := newRig(t, nil)
	session := &http.Cookie{Name: sessionCookieName, Value: r.sess.Issue(testDID)}
	w := r.do(req{method: "POST", target: "/oauth/logout", cookies: []*http.Cookie{session}})
	if w.Code != http.StatusSeeOther || location(w) != "/me" {
		t.Fatalf("%d %q", w.Code, location(w))
	}
	if c := cookieNamed(w, sessionCookieName); c == nil || c.MaxAge >= 0 || c.Value != "" || c.Path != "/" || !c.Secure {
		t.Errorf("cookie %+v", c)
	}
	// Another site can't sign this browser out (it can't do much, but it is not its call).
	w = r.do(req{method: "POST", target: "/oauth/logout", header: map[string]string{"Origin": "https://evil.test"}, cookies: []*http.Cookie{session}})
	if w.Code != http.StatusForbidden || cookieNamed(w, sessionCookieName) != nil {
		t.Errorf("cross-site: %d %v", w.Code, w.Result().Cookies())
	}
	if w := r.do(req{method: "GET", target: "/oauth/logout"}); w.Code != http.StatusMethodNotAllowed {
		t.Errorf("GET: %d", w.Code)
	}
}

func TestMe(t *testing.T) {
	r := newRig(t, nil)
	good := &http.Cookie{Name: sessionCookieName, Value: r.sess.Issue(testDID)}
	other := &http.Cookie{Name: sessionCookieName, Value: r.sess.Issue("did:plc:bbbbbbbbbbbbbbbbbbbbbbbb")}
	tampered := &http.Cookie{Name: sessionCookieName, Value: strings.Replace(good.Value, "v1.", "v1.A", 1)}
	wrongName := &http.Cookie{Name: "feeds_session", Value: good.Value}

	for name, tc := range map[string]struct {
		cookies []*http.Cookie
		code    int
		did     string
		handle  string
	}{
		"signed in":                     {[]*http.Cookie{good}, 200, testDID, "alice.example.test"},
		"an account with no handle":     {[]*http.Cookie{other}, 200, "did:plc:bbbbbbbbbbbbbbbbbbbbbbbb", ""},
		"no cookie":                     {nil, 401, "", ""},
		"a tampered cookie":             {[]*http.Cookie{tampered}, 401, "", ""},
		"the cookie under another name": {[]*http.Cookie{wrongName}, 401, "", ""},
	} {
		w := r.do(req{method: "GET", target: "/api/me", cookies: tc.cookies})
		if w.Code != tc.code || w.Header().Get("Cache-Control") != "no-store" || !strings.HasPrefix(w.Header().Get("Content-Type"), "application/json") {
			t.Errorf("%s: %d, %v", name, w.Code, w.Header())
		}
		var got map[string]string
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil {
			t.Errorf("%s: %v: %s", name, err, w.Body.String())
		}
		if got["did"] != tc.did || got["handle"] != tc.handle {
			t.Errorf("%s: %v", name, got)
		}
	}

	r.clk.Advance(25 * time.Hour)
	if w := r.do(req{method: "GET", target: "/api/me", cookies: []*http.Cookie{good}}); w.Code != 401 {
		t.Errorf("an expired cookie: %d", w.Code)
	}
}

func TestMeSaysWhetherTheOwnerIsSignedIn(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Owner = testDID })
	other := "did:plc:bbbbbbbbbbbbbbbbbbbbbbbb"
	for _, tc := range []struct {
		did   string
		owner bool
	}{{testDID, true}, {other, false}} {
		w := r.do(req{method: "GET", target: "/api/me", cookies: []*http.Cookie{{Name: sessionCookieName, Value: r.sess.Issue(tc.did)}}})
		var got map[string]any
		if err := json.Unmarshal(w.Body.Bytes(), &got); err != nil || w.Code != 200 {
			t.Fatalf("%s: %d %s", tc.did, w.Code, w.Body.String())
		}
		owner, said := got["owner"]
		if tc.owner && owner != true {
			t.Errorf("the owner: %v", got)
		}
		if !tc.owner && said {
			t.Errorf("someone else is told about owner at all: %v", got)
		}
	}
	// With no owner set, nobody is the owner.
	r = newRig(t, nil)
	w := r.do(req{method: "GET", target: "/api/me", cookies: []*http.Cookie{{Name: sessionCookieName, Value: r.sess.Issue(testDID)}}})
	if strings.Contains(w.Body.String(), "owner") {
		t.Errorf("no owner configured: %s", w.Body.String())
	}
}

func TestViewerComesOnlyFromTheCookie(t *testing.T) {
	r := newRig(t, nil)
	cookie := &http.Cookie{Name: sessionCookieName, Value: r.sess.Issue(testDID)}
	hr := httptest.NewRequest("GET", "/x?did=did:plc:victim&viewer=did:plc:victim", nil)
	hr.Header.Set("X-Viewer", "did:plc:victim")
	hr.Header.Set("Authorization", "Bearer did:plc:victim")
	if did, ok := r.h.Viewer(hr); ok {
		t.Errorf("anonymous request, but the viewer is %q", did)
	}
	hr.AddCookie(cookie)
	if did, ok := r.h.Viewer(hr); !ok || did != testDID {
		t.Errorf("%q, %v: with a cookie, it is the cookie's account whatever else the request says", did, ok)
	}
}

func TestHandleLookupIsOptional(t *testing.T) {
	r := newRig(t, func(c *Config) { c.Handle = nil })
	cookie := &http.Cookie{Name: sessionCookieName, Value: r.sess.Issue(testDID)}
	w := r.do(req{method: "GET", target: "/api/me", cookies: []*http.Cookie{cookie}})
	if w.Code != 200 || !strings.Contains(w.Body.String(), testDID) {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
}

func TestLocalhostOriginWorksOverHTTP(t *testing.T) {
	r := newRig(t, func(c *Config) {
		c.Origin = "http://localhost:8710"
		c.Metadata = ClientMetadataFor(c.Origin, "x")
	})
	w := login(r, "alice.example.test")
	c := cookieNamed(w, "feeds_login")
	if c == nil || c.Secure || !c.HttpOnly {
		t.Fatalf("state cookie %+v: over http it can't be Secure or use the __Host- prefix, but it is still HttpOnly", c)
	}
	w = finishLogin(r, "code=abc&state=state-123&iss=x", &http.Cookie{Name: "feeds_login", Value: "state-123"})
	if s := cookieNamed(w, "feeds_session"); s == nil || s.Secure || !s.HttpOnly {
		t.Errorf("session cookie %+v", s)
	}
}

func TestNewChecksItsConfig(t *testing.T) {
	sess, _ := newSessions(t)
	ok := Config{Origin: testOrigin, Auth: newFakeAuth(), Sessions: sess}
	for name, mutate := range map[string]func(*Config){
		"plain http":               func(c *Config) { c.Origin = "http://feeds.example.test" },
		"a path":                   func(c *Config) { c.Origin = testOrigin + "/feeds" },
		"a trailing slash":         func(c *Config) { c.Origin = testOrigin + "/" },
		"no scheme":                func(c *Config) { c.Origin = "feeds.example.test" },
		"credentials":              func(c *Config) { c.Origin = "https://user@feeds.example.test" },
		"a query":                  func(c *Config) { c.Origin = testOrigin + "?x=1" },
		"empty":                    func(c *Config) { c.Origin = "" },
		"no authenticator":         func(c *Config) { c.Auth = nil },
		"no sessions":              func(c *Config) { c.Sessions = nil },
		"http on a lookalike host": func(c *Config) { c.Origin = "http://localhost.evil.test" },
	} {
		cfg := ok
		mutate(&cfg)
		if _, err := New(cfg); err == nil {
			t.Errorf("accepted %s", name)
		}
	}
	if _, err := New(ok); err != nil {
		t.Error(err)
	}
	for _, o := range []string{"http://localhost", "http://localhost:8710", "http://127.0.0.1:8710", "https://feeds.example.test", "https://feeds.example.test:8443"} {
		cfg := ok
		cfg.Origin = o
		if _, err := New(cfg); err != nil {
			t.Errorf("%s: %v", o, err)
		}
	}
}

func TestSameOrigin(t *testing.T) {
	for name, tc := range map[string]struct {
		header map[string]string
		want   bool
	}{
		"origin matches":                 {map[string]string{"Origin": testOrigin}, true},
		"origin differs":                 {map[string]string{"Origin": "https://evil.test"}, false},
		"origin wins over a lying fetch": {map[string]string{"Origin": "https://evil.test", "Sec-Fetch-Site": "same-origin"}, false},
		"fetch same-origin":              {map[string]string{"Sec-Fetch-Site": "same-origin"}, true},
		"fetch none":                     {map[string]string{"Sec-Fetch-Site": "none"}, true},
		"fetch cross-site":               {map[string]string{"Sec-Fetch-Site": "cross-site"}, false},
		"fetch same-site":                {map[string]string{"Sec-Fetch-Site": "same-site"}, false},
		"neither (a tool, not a page)":   {nil, true},
	} {
		hr := httptest.NewRequest("POST", "/", nil)
		for k, v := range tc.header {
			hr.Header.Set(k, v)
		}
		if got := SameOrigin(hr, testOrigin); got != tc.want {
			t.Errorf("%s: %v, want %v", name, got, tc.want)
		}
	}
}
