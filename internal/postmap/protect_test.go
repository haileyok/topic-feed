package postmap

import (
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func newLimitedServer(t *testing.T, l *Limits) *httptest.Server {
	t.Helper()
	root := t.TempDir()
	writeSnapshot(t, root, testSnap)
	if err := os.Symlink(testSnap, filepath.Join(root, "latest")); err != nil {
		t.Fatal(err)
	}
	s := &Server{Dir: root, Log: slog.New(slog.NewTextHandler(io.Discard, nil)), Limits: l}
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// getAs asks as a client the tunnel would have named in CF-Connecting-IP (the test server is on the loopback address).
func getAs(t *testing.T, url, client string) (int, http.Header) {
	t.Helper()
	req, _ := http.NewRequest("GET", url, nil)
	if client != "" {
		req.Header.Set("CF-Connecting-IP", client)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, resp.Header
}

func TestABucketRunsOutAndFillsUpAgain(t *testing.T) {
	t.Parallel()
	now := time.Unix(1000, 0)
	l := &Limits{now: func() time.Time { return now }}
	for i := 0; i < 3; i++ {
		if ok, _ := l.allow("a", "k", 1, 3); !ok {
			t.Fatalf("request %d within the burst was refused", i)
		}
	}
	ok, wait := l.allow("a", "k", 1, 3)
	if ok || wait <= 0 || wait > 2*time.Second {
		t.Fatalf("a fourth request should wait about a second, got ok=%v wait=%v", ok, wait)
	}
	if ok, _ := l.allow("b", "k", 1, 3); !ok {
		t.Error("another client has its own bucket")
	}
	if ok, _ := l.allow("a", "other", 1, 3); !ok {
		t.Error("another kind of request has its own bucket")
	}
	now = now.Add(1500 * time.Millisecond)
	if ok, _ := l.allow("a", "k", 1, 3); !ok {
		t.Error("after a second and a half one request is allowed again")
	}
	if ok, _ := l.allow("a", "k", 1, 3); ok {
		t.Error("but only one")
	}
}

func TestOnlyTheTunnelsAddressHeaderIsBelieved(t *testing.T) {
	t.Parallel()
	req := func(remote, header string) *http.Request {
		r := httptest.NewRequest("GET", "/", nil)
		r.RemoteAddr = remote
		if header != "" {
			r.Header.Set("CF-Connecting-IP", header)
		}
		return r
	}
	for _, c := range []struct{ remote, header, want string }{
		{"127.0.0.1:5555", "203.0.113.7", "203.0.113.7"}, // from the tunnel on this machine
		{"[::1]:5555", "2001:db8::1", "2001:db8::1"},
		{"127.0.0.1:5555", "", "127.0.0.1"},
		{"127.0.0.1:5555", "not an address", "127.0.0.1"},
		{"198.51.100.9:5555", "203.0.113.7", "198.51.100.9"}, // from anywhere else, the header is made up
	} {
		if got := clientIP(req(c.remote, c.header), false); got != c.want {
			t.Errorf("%s with header %q: %s, want %s", c.remote, c.header, got, c.want)
		}
	}
}

func TestTheHeaderCanBeBelievedFromAnyPeerWhenTheOperatorSaysSo(t *testing.T) {
	t.Parallel()
	r := httptest.NewRequest("GET", "/", nil)
	r.RemoteAddr = "172.18.0.1:5555" // a container's gateway: where the tunnel's connections arrive from
	r.Header.Set("CF-Connecting-IP", "203.0.113.7")
	if got := clientIP(r, true); got != "203.0.113.7" {
		t.Errorf("trusted: %s", got)
	}
	if got := clientIP(r, false); got != "172.18.0.1" {
		t.Errorf("not trusted: %s", got)
	}
}

func TestTooManyRequestsFromOneClientAreRefusedAndOthersAreNot(t *testing.T) {
	t.Parallel()
	ts := newLimitedServer(t, &Limits{PostsPerSec: 0.001, PostsBurst: 3, SearchPerSec: 0.001, SearchBurst: 2, SearchesAtOnce: 4, MaxQueryChars: 50})
	var codes []int
	for i := 0; i < 5; i++ {
		c, h := getAs(t, ts.URL+"/api/v1/post/1", "203.0.113.1")
		codes = append(codes, c)
		if c == 429 && h.Get("Retry-After") == "" {
			t.Error("a refusal should say when to come back")
		}
	}
	if codes[2] != 200 || codes[3] != 429 || codes[4] != 429 {
		t.Errorf("posts: %v, want three allowed then refused", codes)
	}
	if c, _ := getAs(t, ts.URL+"/api/v1/post/1", "203.0.113.2"); c != 200 {
		t.Errorf("another client: %d", c)
	}
	var sc []int
	for i := 0; i < 3; i++ {
		c, _ := getAs(t, ts.URL+"/api/v1/search?q=quick", "203.0.113.1")
		sc = append(sc, c)
	}
	if sc[1] != 200 || sc[2] != 429 {
		t.Errorf("searches: %v, want two allowed then refused", sc)
	}
	if c, _ := getAs(t, ts.URL+"/healthz", "203.0.113.1"); c != 200 {
		t.Errorf("the health check is never limited: %d", c)
	}
	if c, _ := getAs(t, ts.URL+"/", "203.0.113.1"); c != 200 {
		t.Errorf("the page is never limited: %d", c)
	}
}

func TestSearchesRunningAtOnceAreCappedAndLongQueriesRefused(t *testing.T) {
	t.Parallel()
	l := &Limits{PostsPerSec: 100, PostsBurst: 100, SearchPerSec: 100, SearchBurst: 100, SearchesAtOnce: 1, MaxQueryChars: 20}
	ts := newLimitedServer(t, l)
	release := l.startSearch()
	if release == nil {
		t.Fatal("the first place should be free")
	}
	if l.startSearch() != nil {
		t.Fatal("with one place, a second search should have to wait")
	}
	c, h := getAs(t, ts.URL+"/api/v1/search?q=quick", "")
	if c != 503 || h.Get("Retry-After") == "" {
		t.Errorf("a search while the place is taken: %d", c)
	}
	release()
	if c, _ := getAs(t, ts.URL+"/api/v1/search?q=quick", ""); c != 200 {
		t.Errorf("a search once it is free: %d", c)
	}
	if c, _ := getAs(t, ts.URL+"/api/v1/search?q="+strings.Repeat("a", 21), ""); c != 400 {
		t.Errorf("a long query: %d, want 400", c)
	}
	if c, _ := getAs(t, ts.URL+"/api/v1/search?q="+strings.Repeat("a", 20), ""); c != 200 {
		t.Errorf("a query at the limit: %d", c)
	}
}

func TestEverythingCarriesSecurityHeadersAndRobotsKeepsCrawlersOffTheData(t *testing.T) {
	t.Parallel()
	ts := newLimitedServer(t, DefaultLimits())
	for _, p := range []string{"/", "/api/latest", "/api/v1/post/0", "/healthz", "/robots.txt", "/snap/" + testSnap + "/map.json", "/nope"} {
		_, h := getAs(t, ts.URL+p, "")
		csp := h.Get("Content-Security-Policy")
		if !strings.Contains(csp, "default-src 'self'") || !strings.Contains(csp, "frame-ancestors 'none'") {
			t.Errorf("%s: CSP %q", p, csp)
		}
		if h.Get("X-Content-Type-Options") != "nosniff" || h.Get("Referrer-Policy") != "no-referrer" || h.Get("X-Frame-Options") != "DENY" {
			t.Errorf("%s: missing headers %v", p, h)
		}
	}
	_, body, _ := get(t, ts.URL+"/robots.txt")
	if !strings.Contains(string(body), "Disallow: /api/") || !strings.Contains(string(body), "Disallow: /snap/") {
		t.Errorf("robots.txt: %q", body)
	}
}
