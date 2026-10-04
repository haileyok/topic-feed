package signin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

// browserJar is a cookie jar that applies the rules a real browser does when it is sent a
// Set-Cookie, which Go's own jar and httptest don't. The one that matters most here: a cookie
// named with the __Host- prefix is kept only if it is Secure, has path "/", and has no Domain.
// A browser that is sent one that isn't drops it without a word, so a login whose cookie
// breaks the rule can never finish, and no test that just reads the header notices.
type browserJar struct {
	cookies  []*http.Cookie
	rejected []string // why each refused cookie was refused
}

// accepts reports why a browser would refuse to keep this cookie, or "".
func accepts(u *url.URL, c *http.Cookie) string {
	switch {
	case strings.HasPrefix(c.Name, "__Host-") && (!c.Secure || c.Path != "/" || c.Domain != ""):
		return fmt.Sprintf("%s: a __Host- cookie must be Secure, with path / and no Domain (Secure=%v Path=%q Domain=%q)", c.Name, c.Secure, c.Path, c.Domain)
	case strings.HasPrefix(c.Name, "__Secure-") && !c.Secure:
		return c.Name + ": a __Secure- cookie must be Secure"
	case c.Secure && u.Scheme != "https":
		return c.Name + ": a Secure cookie can only be set over https"
	case c.SameSite == http.SameSiteNoneMode && !c.Secure:
		return c.Name + ": SameSite=None needs Secure"
	}
	return ""
}

// store keeps what a response sets, as a browser would.
func (j *browserJar) store(u *url.URL, w *httptest.ResponseRecorder) {
	for _, c := range w.Result().Cookies() {
		if why := accepts(u, c); why != "" {
			j.rejected = append(j.rejected, why)
			continue
		}
		path := c.Path
		if path == "" {
			path = "/"
		}
		kept := j.cookies[:0:0]
		for _, old := range j.cookies {
			oldPath := old.Path
			if oldPath == "" {
				oldPath = "/"
			}
			if old.Name != c.Name || oldPath != path { // the same name and path is replaced
				kept = append(kept, old)
			}
		}
		j.cookies = kept
		if c.MaxAge >= 0 { // a negative MaxAge, as Go parses Max-Age=0, deletes
			j.cookies = append(j.cookies, c)
		}
	}
}

// cookiesFor are the cookies a browser sends with a request to u.
func (j *browserJar) cookiesFor(u *url.URL) []*http.Cookie {
	var out []*http.Cookie
	for _, c := range j.cookies {
		path := c.Path
		if path == "" {
			path = "/"
		}
		matches := u.Path == path || strings.HasPrefix(u.Path, strings.TrimSuffix(path, "/")+"/")
		if matches && (!c.Secure || u.Scheme == "https") {
			out = append(out, c)
		}
	}
	return out
}

func (j *browserJar) has(name string) bool {
	for _, c := range j.cookies {
		if c.Name == name {
			return true
		}
	}
	return false
}

// visit makes a request as the browser: carrying the jar's cookies, and keeping what comes back.
func (r *rig) visit(j *browserJar, method, target string, form url.Values, header map[string]string) *httptest.ResponseRecorder {
	r.t.Helper()
	u, err := url.Parse(r.h.Origin() + target)
	if err != nil {
		r.t.Fatal(err)
	}
	var body *strings.Reader
	if form != nil {
		body = strings.NewReader(form.Encode())
	} else {
		body = strings.NewReader("")
	}
	hr := httptest.NewRequest(method, target, body)
	hr.Host = u.Host
	if form != nil {
		hr.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	}
	if method == "POST" {
		hr.Header.Set("Origin", r.h.Origin()) // browsers say where a POST comes from
	}
	for k, v := range header {
		hr.Header.Set(k, v)
	}
	for _, c := range j.cookiesFor(u) {
		hr.AddCookie(c)
	}
	w := httptest.NewRecorder()
	r.mux.ServeHTTP(w, hr)
	j.store(u, w)
	return w
}

func (j *browserJar) mustHaveRefusedNothing(t *testing.T) {
	t.Helper()
	for _, why := range j.rejected {
		t.Errorf("a browser would have thrown this cookie away: %s", why)
	}
}

func TestTheJarEnforcesTheRulesABrowserDoes(t *testing.T) {
	u, _ := url.Parse("https://feeds.example.test/oauth/login")
	http1, _ := url.Parse("http://feeds.example.test/oauth/login")
	for name, tc := range map[string]struct {
		url    *url.URL
		cookie *http.Cookie
		want   bool
	}{
		"a proper __Host- cookie":            {u, &http.Cookie{Name: "__Host-x", Secure: true, Path: "/"}, true},
		"a __Host- cookie with another path": {u, &http.Cookie{Name: "__Host-x", Secure: true, Path: "/oauth"}, false},
		"a __Host- cookie with no path":      {u, &http.Cookie{Name: "__Host-x", Secure: true}, false},
		"a __Host- cookie that isn't Secure": {u, &http.Cookie{Name: "__Host-x", Path: "/"}, false},
		"a __Host- cookie with a Domain":     {u, &http.Cookie{Name: "__Host-x", Secure: true, Path: "/", Domain: "example.test"}, false},
		"a __Secure- cookie, not Secure":     {u, &http.Cookie{Name: "__Secure-x", Path: "/"}, false},
		"a Secure cookie over http":          {http1, &http.Cookie{Name: "x", Secure: true, Path: "/"}, false},
		"SameSite=None without Secure":       {u, &http.Cookie{Name: "x", Path: "/", SameSite: http.SameSiteNoneMode}, false},
		"an ordinary cookie":                 {http1, &http.Cookie{Name: "x", Path: "/oauth"}, true},
	} {
		if got := accepts(tc.url, tc.cookie) == ""; got != tc.want {
			t.Errorf("%s: accepted=%v, want %v", name, got, tc.want)
		}
	}
}

// The login, start to finish, in a browser that enforces the cookie rules.
func TestALoginFinishesInABrowserThatEnforcesCookieRules(t *testing.T) {
	for _, mode := range []struct {
		name   string
		header map[string]string
	}{
		{"the page's script (JSON)", asJSON},
		{"a plain form post (redirects)", nil},
	} {
		t.Run(mode.name, func(t *testing.T) {
			r := newRig(t, nil)
			jar := &browserJar{}

			w := r.visit(jar, "POST", "/oauth/login", url.Values{"handle": {"alice.example.test"}}, mode.header)
			if w.Code != 200 && w.Code != http.StatusSeeOther {
				t.Fatalf("login: %d", w.Code)
			}
			if !jar.has(stateCookieName) {
				t.Fatalf("the browser did not keep the cookie that ties the login to it (it refused: %v)", jar.rejected)
			}
			// Their own server sends them back to the callback, a different path.
			w = r.visit(jar, "GET", "/oauth/callback?code=abc&state=state-123&iss=x", nil, nil)
			if location(w) != "/me" {
				t.Fatalf("callback went to %q: the browser should have been recognised as the one that started the login (log: %v)", location(w), r.auth.finished())
			}
			if len(r.auth.finished()) != 1 {
				t.Fatalf("the login was not finished: %v", r.auth.finished())
			}
			if jar.has(stateCookieName) {
				t.Error("the browser still holds the login's cookie after it finished")
			}
			if !jar.has(sessionCookieName) {
				t.Fatalf("the browser did not keep the session (it refused: %v)", jar.rejected)
			}
			w = r.visit(jar, "GET", "/api/me", nil, nil)
			if w.Code != 200 || !strings.Contains(w.Body.String(), testDID) {
				t.Errorf("/api/me: %d %s", w.Code, w.Body.String())
			}
			w = r.visit(jar, "POST", "/oauth/logout", nil, mode.header)
			if jar.has(sessionCookieName) {
				t.Errorf("signing out left the session cookie in the browser (logout %d)", w.Code)
			}
			if w := r.visit(jar, "GET", "/api/me", nil, nil); w.Code != http.StatusUnauthorized {
				t.Errorf("/api/me after signing out: %d", w.Code)
			}
			jar.mustHaveRefusedNothing(t)
		})
	}
}

// Whatever else a login does, it never sets a cookie a browser would refuse.
func TestNoResponseSetsACookieABrowserWouldRefuse(t *testing.T) {
	run := map[string]func(r *rig, jar *browserJar){
		"login": func(r *rig, j *browserJar) {
			r.visit(j, "POST", "/oauth/login", url.Values{"handle": {"alice.example.test"}}, nil)
		},
		"login, JSON": func(r *rig, j *browserJar) {
			r.visit(j, "POST", "/oauth/login", url.Values{"handle": {"alice.example.test"}}, asJSON)
		},
		"login that can't start": func(r *rig, j *browserJar) {
			r.auth.startErr = ErrBusy
			r.visit(j, "POST", "/oauth/login", url.Values{"handle": {"a.test"}}, nil)
		},
		"callback with no login": func(r *rig, j *browserJar) { r.visit(j, "GET", "/oauth/callback?code=a&state=b", nil, nil) },
		"callback denied": func(r *rig, j *browserJar) {
			r.visit(j, "POST", "/oauth/login", url.Values{"handle": {"alice.example.test"}}, asJSON)
			r.visit(j, "GET", "/oauth/callback?error=access_denied&state=state-123", nil, nil)
		},
		"callback that fails": func(r *rig, j *browserJar) {
			r.auth.finishErr = ErrBusy
			r.visit(j, "POST", "/oauth/login", url.Values{"handle": {"alice.example.test"}}, asJSON)
			r.visit(j, "GET", "/oauth/callback?code=a&state=state-123", nil, nil)
		},
		"logout": func(r *rig, j *browserJar) { r.visit(j, "POST", "/oauth/logout", nil, nil) },
	}
	for name, f := range run {
		r := newRig(t, nil)
		jar := &browserJar{}
		f(r, jar)
		if len(jar.rejected) != 0 {
			t.Errorf("%s: %v", name, jar.rejected)
		}
	}
}
