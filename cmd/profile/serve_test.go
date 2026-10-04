package main

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

	"github.com/haileyok/topic-feed/internal/feedgen"
)

var t0 = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type fakeSource struct {
	mu    sync.Mutex
	likes feedgen.LikeData
	liked []feedgen.LikedPost
	cov   feedgen.LikeCoverage
	err   error
	calls int
}

func (f *fakeSource) LikeProfile(context.Context, string, time.Time, time.Time, time.Duration) (feedgen.LikeData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.calls++
	return f.likes, f.err
}

func (f *fakeSource) LikedPosts(context.Context, string, time.Time) ([]feedgen.LikedPost, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.liked, f.err
}

func (f *fakeSource) LikeCoverage(context.Context, string, time.Time) (feedgen.LikeCoverage, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.cov, f.err
}

func (f *fakeSource) lookups() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

func resolve(_ context.Context, actor string) (string, string, error) {
	switch actor {
	case "me.test", "did:plc:me":
		return "did:plc:me", "me.test", nil
	}
	return "", "", errors.New("no such account")
}

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func liked(topic, text string, n int) feedgen.LikedPost {
	return feedgen.LikedPost{URI: "at://did:plc:author/app.bsky.feed.post/" + topic[len(topic)-2:] + string(rune('a'+n)),
		LikedAt: t0.Add(-time.Duration(n) * time.Hour), Text: text, TopPath: topic, TopP: 0.9}
}

// sample data: AI and cats liked most, a topic with probability but no liked posts filed
// under it, and a few posts filed elsewhere.
func newSource() *fakeSource {
	var posts []feedgen.LikedPost
	for i := range 10 {
		posts = append(posts, liked("technology/ai", "AI post "+string(rune('A'+i)), i))
	}
	posts = append(posts, liked("animals_nature/cats", "a cat", 11), liked("animals_nature/cats", "another cat", 12))
	posts = append(posts, liked("unclear", "who knows", 13))
	posts = append(posts, liked("sports/baseball", "ball", 14), liked("sports/baseball", "bat", 15))
	return &fakeSource{
		likes: feedgen.LikeData{Mass: map[string]float64{"technology/ai": 3, "animals_nature/cats": 1, "unclear": 9, "humor/shitposts": 0.5}, Posts: 15},
		liked: posts,
		cov:   feedgen.LikeCoverage{Total: 80, Classified: 15, Unclassified: 5, RepliesOrOther: 50, Unseen: 10},
	}
}

var names = map[string]feedgen.TopicName{
	"technology/ai":       {Name: "AI", Broad: "Technology"},
	"animals_nature/cats": {Name: "Cats", Broad: "Animals & nature"},
	// humor/shitposts is missing on purpose: the page falls back to the path.
}

func newServer(src feedgen.InterestsSource, defaultActor string) (*pageServer, *clock) {
	p := newPageServer(src, resolve, feedgen.PersonalConfigDefaults(), names, defaultActor, slog.New(slog.NewTextHandler(io.Discard, nil)))
	c := &clock{t: t0}
	p.now = c.Now
	return p, c
}

func get(t *testing.T, p *pageServer, target string) (int, http.Header, []byte) {
	t.Helper()
	rec := httptest.NewRecorder()
	p.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, target, nil))
	return rec.Code, rec.Header(), rec.Body.Bytes()
}

func fetchProfile(t *testing.T, p *pageServer, actor string) feedgen.InterestsResponse {
	t.Helper()
	code, _, body := get(t, p, "/api/profile?actor="+url.QueryEscape(actor))
	if code != 200 {
		t.Fatalf("%s: %d %s", actor, code, body)
	}
	var r feedgen.InterestsResponse
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	return r
}

func TestProfileInterests(t *testing.T) {
	p, _ := newServer(newSource(), "")
	r := fetchProfile(t, p, "@me.test") // a leading @ is fine
	if r.DID != "did:plc:me" || r.Handle != "me.test" || !r.Personalized {
		t.Errorf("%+v", r)
	}
	// The interests are the feed's: shares of the mass, "unclear" left out.
	if len(r.Interests) != 3 {
		t.Fatalf("%d interests: %+v", len(r.Interests), r.Interests)
	}
	want := []struct {
		path, name, broad string
		share             float64
		posts, samples    int
	}{
		{"technology/ai", "AI", "Technology", 3 / 4.5, 10, 8},
		{"animals_nature/cats", "Cats", "Animals & nature", 1 / 4.5, 2, 2},
		{"humor/shitposts", "humor/shitposts", "", 0.5 / 4.5, 0, 0},
	}
	total := 0.0
	for i, w := range want {
		in := r.Interests[i]
		if in.Path != w.path || in.Name != w.name || in.Broad != w.broad || abs(in.Share-w.share) > 1e-9 || in.Posts != w.posts || len(in.Samples) != w.samples {
			t.Errorf("interest %d: %+v, want %+v", i, in, w)
		}
		total += in.Share
	}
	if abs(total-1) > 1e-9 {
		t.Errorf("shares add up to %v", total)
	}
	// The newest liked posts first, as the database returned them.
	if s := r.Interests[0].Samples; s[0].Text != "AI post A" || s[7].Text != "AI post H" || s[0].P != float64(float32(0.9)) ||
		!s[0].LikedAt.Equal(t0) || s[0].URL != "https://bsky.app/profile/did:plc:author/post/aia" {
		t.Errorf("samples %+v", s)
	}
	if r.OtherPosts != 2 || r.UnclearPosts != 1 {
		t.Errorf("other %d, unclear %d, want 2 and 1", r.OtherPosts, r.UnclearPosts)
	}
	if c := r.Coverage; c.Total != 80 || c.Classified != 15 || c.Unclassified != 5 || c.RepliesOrOther != 50 || c.Unseen != 10 {
		t.Errorf("coverage %+v", c)
	}
	if s := r.Settings; s.LookbackDays != 30 || s.HalfLifeDays != 7 || s.MinLikes != 5 || s.Topics != 20 {
		t.Errorf("settings %+v", s)
	}
}

func TestProfileWithTooFewLikesIsNotPersonalized(t *testing.T) {
	src := newSource()
	src.likes.Posts = 2
	p, _ := newServer(src, "")
	r := fetchProfile(t, p, "me.test")
	if r.Personalized {
		t.Error("two classified likes are fewer than the feed needs")
	}
	if len(r.Interests) == 0 {
		t.Error("the interests are still shown")
	}
}

func TestProfileErrors(t *testing.T) {
	p, _ := newServer(newSource(), "")
	code, hdr, body := get(t, p, "/api/profile?actor=nobody")
	if code != 404 || !strings.Contains(string(body), `Couldn't find the account \"nobody\"`) || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
		t.Errorf("unknown account: %d %s", code, body)
	}
	if code, _, body := get(t, p, "/api/profile"); code != 400 || !strings.Contains(string(body), "Enter a handle or DID") {
		t.Errorf("no account and no default: %d %s", code, body)
	}

	// With a default account, none named means that one.
	p, _ = newServer(newSource(), "me.test")
	if r := fetchProfile(t, p, ""); r.DID != "did:plc:me" {
		t.Errorf("default account: %+v", r)
	}

	// A database failure is a plain error, not the database's own words.
	broken := newSource()
	broken.err = errors.New("clickhouse says: secret internals")
	p, _ = newServer(broken, "")
	code, _, body = get(t, p, "/api/profile?actor=me.test")
	if code != 500 || strings.Contains(string(body), "secret") || !strings.Contains(string(body), "Couldn't read") {
		t.Errorf("source error: %d %s", code, body)
	}
}

func TestProfileTextIsDataNotMarkup(t *testing.T) {
	src := newSource()
	evil := `<img src=x onerror=alert(1)> & "quotes"`
	src.liked = []feedgen.LikedPost{{URI: "not a uri", LikedAt: t0, Text: "  spaced \n\t out  " + evil, TopPath: "technology/ai", TopP: 0.5}}
	p, _ := newServer(src, "")
	code, hdr, body := get(t, p, "/api/profile?actor=me.test")
	if code != 200 || hdr.Get("Content-Type") != "application/json" || strings.Contains(string(body), "<img") {
		t.Fatalf("%d %s: markup must not appear raw in the JSON", code, body)
	}
	var r feedgen.InterestsResponse
	json.Unmarshal(body, &r)
	s := r.Interests[0].Samples[0]
	if s.Text != "spaced out "+evil || s.URL != "" {
		t.Errorf("text %q url %q: whitespace collapsed, the rest verbatim, no link for a bad URI", s.Text, s.URL)
	}
}

func TestProfileIsCachedBriefly(t *testing.T) {
	src := newSource()
	p, clk := newServer(src, "")
	fetchProfile(t, p, "me.test")
	fetchProfile(t, p, "did:plc:me") // the same account by DID
	if n := src.lookups(); n != 1 {
		t.Fatalf("%d lookups for two requests, want 1", n)
	}
	clk.Advance(cacheFor + time.Second)
	fetchProfile(t, p, "me.test")
	if n := src.lookups(); n != 2 {
		t.Errorf("%d lookups after the cache expired, want 2", n)
	}
}

func TestProfileSaysBusyRatherThanQueueingForever(t *testing.T) {
	p, _ := newServer(newSource(), "")
	for range maxLookups {
		p.slots <- struct{}{} // every lookup slot taken
	}
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	rec := httptest.NewRecorder()
	p.routes().ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/api/profile?actor=me.test", nil).WithContext(ctx))
	if rec.Code != 503 || !strings.Contains(rec.Body.String(), "Busy") {
		t.Errorf("%d %s", rec.Code, rec.Body)
	}
}

func TestPageEscapesTheDefaultAccountAndSetsCSP(t *testing.T) {
	p, _ := newServer(newSource(), `"><script>alert(1)</script>`)
	code, hdr, body := get(t, p, "/")
	page := string(body)
	if code != 200 || !strings.HasPrefix(hdr.Get("Content-Type"), "text/html") {
		t.Fatalf("%d %s", code, hdr.Get("Content-Type"))
	}
	if strings.Contains(page, `"><script>alert(1)</script>`) || !strings.Contains(page, "&#34;&gt;&lt;script&gt;alert(1)&lt;/script&gt;") {
		t.Error("the starting account must be HTML-escaped into the page")
	}
	if strings.Contains(page, "__DEFAULT_ACTOR__") {
		t.Error("placeholder left in the page")
	}
	if csp := hdr.Get("Content-Security-Policy"); !strings.Contains(csp, "default-src 'none'") || !strings.Contains(csp, "connect-src 'self'") {
		t.Errorf("CSP %q", csp)
	}
	if code, _, _ := get(t, p, "/nope"); code != 404 {
		t.Errorf("/nope: %d", code)
	}
}

func abs(x float64) float64 {
	if x < 0 {
		return -x
	}
	return x
}
