package feedgen

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/haileyok/topic-feed/internal/taxonomy"
)

// recordingBuilder remembers the feeds it was asked to build.
type recordingBuilder struct {
	mu     sync.Mutex
	feeds  []Feed
	sinces []time.Time
}

func (b *recordingBuilder) Build(_ context.Context, f Feed, since time.Time, _ int) ([]Post, Removed, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.feeds = append(b.feeds, f)
	b.sinces = append(b.sinces, since)
	return posts(3), Removed{}, nil
}

func (b *recordingBuilder) last() Feed {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.feeds[len(b.feeds)-1]
}

const testAdultKey = "a-long-enough-private-key-for-tests"

func adultTestServer(t *testing.T) (*Server, *recordingBuilder) {
	tax, err := taxonomy.Load("../../taxonomy/v1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	s := testServer(t)
	rb := &recordingBuilder{}
	pv := NewPreviewer(rb, tax, slog.New(slog.NewTextHandler(io.Discard, nil)))
	pv.AdultKey = testAdultKey
	s.Preview = pv
	return s, rb
}

func do(s *Server, method, path, body, cookie string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/json")
	if cookie != "" {
		req.Header.Set("Cookie", cookie)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec
}

func TestAdultAccessIsOwnerOnly(t *testing.T) {
	s, rb := adultTestServer(t)

	if rec := do(s, http.MethodGet, "/adult-access?key=wrong-key-wrong-key-wrong-key", "", ""); rec.Code != http.StatusNotFound {
		t.Fatalf("wrong key: %d, want 404", rec.Code)
	}
	rec := do(s, http.MethodGet, "/adult-access?key="+testAdultKey, "", "")
	if rec.Code != http.StatusSeeOther {
		t.Fatalf("right key: %d, want 303", rec.Code)
	}
	set := rec.Header().Get("Set-Cookie")
	if !strings.HasPrefix(set, adultCookie+"=") || !strings.Contains(set, "HttpOnly") || !strings.Contains(set, "Secure") ||
		strings.Contains(set, testAdultKey) {
		t.Fatalf("cookie %q: want HttpOnly, Secure, and no plain key", set)
	}
	cookie := strings.SplitN(set, ";", 2)[0]

	adult := `{"paths":["adult_content"],"min_prob":0.5,"tone":{},"signals":{},"allow_adult":true}`
	if rec := do(s, http.MethodPost, "/api/preview", adult, ""); rec.Code != http.StatusForbidden {
		t.Errorf("adult preview without the cookie: %d, want 403", rec.Code)
	}
	if rec := do(s, http.MethodPost, "/api/preview", adult, adultCookie+"=forged"); rec.Code != http.StatusForbidden {
		t.Errorf("adult preview with a forged cookie: %d, want 403", rec.Code)
	}
	if rec := do(s, http.MethodPost, "/api/preview", adult, cookie); rec.Code != http.StatusOK {
		t.Fatalf("adult preview with the cookie: %d %s", rec.Code, rec.Body)
	}
	if f := rb.last(); !f.AllowAdult || f.Exclude["adult_content"] != 0 {
		t.Errorf("adult build: AllowAdult %v, exclude %v; want allowed and no adult cutoff", f.AllowAdult, f.Exclude)
	}

	plain := `{"paths":["technology/ai"],"min_prob":0.5,"tone":{},"signals":{}}`
	if rec := do(s, http.MethodPost, "/api/preview", plain, cookie); rec.Code != http.StatusOK {
		t.Fatalf("plain preview: %d", rec.Code)
	}
	if f := rb.last(); f.AllowAdult || f.Exclude["adult_content"] != adultCutoff {
		t.Errorf("without allow_adult the filters stay on, even with the cookie: %+v", f)
	}
	if rec := do(s, http.MethodPost, "/api/preview", `{"paths":["adult_content"],"min_prob":0.5,"tone":{},"signals":{}}`, ""); rec.Code != http.StatusBadRequest {
		t.Errorf("adult topic without allow_adult: %d, want 400", rec.Code)
	}
}

func TestTaxonomyShowsAdultTopicsOnlyWithTheCookie(t *testing.T) {
	s, _ := adultTestServer(t)
	cookie := strings.SplitN(do(s, http.MethodGet, "/adult-access?key="+testAdultKey, "", "").Header().Get("Set-Cookie"), ";", 2)[0]

	hasAdult := func(rec *httptest.ResponseRecorder) (bool, bool) {
		var body struct {
			Topics []struct {
				ID string `json:"id"`
			} `json:"topics"`
			AdultAllowed bool `json:"adult_allowed"`
		}
		json.Unmarshal(rec.Body.Bytes(), &body)
		for _, t := range body.Topics {
			if t.ID == "adult_content" {
				return true, body.AdultAllowed
			}
		}
		return false, body.AdultAllowed
	}
	rec := do(s, http.MethodGet, "/api/taxonomy", "", "")
	if topic, allowed := hasAdult(rec); topic || allowed {
		t.Errorf("public taxonomy: adult topic %v, adult_allowed %v", topic, allowed)
	}
	rec = do(s, http.MethodGet, "/api/taxonomy", "", cookie)
	if topic, allowed := hasAdult(rec); !topic || !allowed {
		t.Errorf("owner taxonomy: adult topic %v, adult_allowed %v", topic, allowed)
	}
	if cc := rec.Header().Get("Cache-Control"); !strings.Contains(cc, "private") {
		t.Errorf("owner taxonomy must not be cached publicly: %q", cc)
	}
	// The topics a filtered feed can leave out are every one, for anybody; that grants nothing else.
	rec = do(s, http.MethodGet, "/api/taxonomy?for=filters", "", "")
	if topic, allowed := hasAdult(rec); !topic || allowed {
		t.Errorf("filters taxonomy: adult topic %v, adult_allowed %v (want true, false)", topic, allowed)
	}

	s.Preview.AdultKey = ""
	if rec := do(s, http.MethodGet, "/adult-access?key=", "", ""); rec.Code != http.StatusNotFound {
		t.Errorf("no key configured: %d, want 404", rec.Code)
	}
	if topic, _ := hasAdult(do(s, http.MethodGet, "/api/taxonomy", "", cookie)); topic {
		t.Error("with no key configured, old cookies grant nothing")
	}
}
