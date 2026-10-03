package pipeline

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func TestClassifySendsPicturesAndReadsPredictions(t *testing.T) {
	var got struct {
		Posts []struct {
			Text     string   `json:"text"`
			Pictures []string `json:"pictures"`
		} `json:"posts"`
	}
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/classify" || r.Method != http.MethodPost {
			t.Errorf("unexpected request %s %s", r.Method, r.URL.Path)
		}
		if err := json.NewDecoder(r.Body).Decode(&got); err != nil {
			t.Error(err)
		}
		results := make([]map[string]any, len(got.Posts))
		for i, p := range got.Posts {
			results[i] = map[string]any{"broad": map[string]float64{"art": 0.9}, "paths": map[string]float64{"art/other": 0.5},
				"signals": map[string]float64{"meme": 0.25}, "tone": map[string]float64{"other": 1}, "pictures_used": len(p.Pictures)}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "v5", "results": results})
	}))
	defer srv.Close()
	c := &Classifier{URL: srv.URL, Client: srv.Client()}
	model, preds, err := c.Classify(context.Background(), []Item{{Text: "a cat"}, {Text: "a dog", Pictures: [][]byte{[]byte("jpeg bytes"), []byte("more")}}})
	if err != nil {
		t.Fatal(err)
	}
	if model != "v5" || len(preds) != 2 {
		t.Fatalf("model %q, %d predictions", model, len(preds))
	}
	if preds[1].Signals["meme"] != 0.25 || preds[1].PicturesUsed != 2 || preds[0].PicturesUsed != 0 {
		t.Errorf("predictions %+v", preds)
	}
	if len(got.Posts[0].Pictures) != 0 || len(got.Posts[1].Pictures) != 2 {
		t.Fatalf("pictures sent: %v", got.Posts)
	}
	if b, err := base64.StdEncoding.DecodeString(got.Posts[1].Pictures[0]); err != nil || string(b) != "jpeg bytes" {
		t.Errorf("pictures are sent as base64: %q %v", got.Posts[1].Pictures[0], err)
	}
}

func TestClassifyStoresMemeOnlyWhenThePictureWasSeen(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		var req struct {
			Posts []Item `json:"posts"`
		}
		_ = json.NewDecoder(r.Body).Decode(&req)
		results := make([]map[string]any, len(req.Posts))
		for i, p := range req.Posts {
			used := len(p.Pictures)
			if p.Text == "unreadable picture" {
				used = 0 // the service could not decode the file
			}
			results[i] = map[string]any{"broad": map[string]float64{"humor": 0.8}, "paths": map[string]float64{"humor/shitposts": 0.6},
				"signals": map[string]float64{"meme": 0.7, "news": 0.1}, "tone": map[string]float64{"humorous": 1}, "pictures_used": used}
		}
		_ = json.NewEncoder(w).Encode(map[string]any{"model": "v5", "results": results})
	}))
	defer srv.Close()
	p := &Pipeline{Classifier: &Classifier{URL: srv.URL, Client: srv.Client()}, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	posts := []post{{Text: "text only"}, {Text: "with a picture"}, {Text: "unreadable picture"}}
	rows := []Row{newRow("a", "d", time.Now(), "ok", nil), newRow("b", "d", time.Now(), "ok", nil), newRow("c", "d", time.Now(), "ok", nil)}
	pics := [][][]byte{nil, {[]byte("jpeg")}, {[]byte("broken")}}
	if err := p.classify(context.Background(), posts, rows, pics); err != nil {
		t.Fatal(err)
	}
	if _, has := rows[0].Signals["meme"]; has || rows[0].PicturesUsed != 0 {
		t.Errorf("text-only post: meme stored %v, pictures used %d", rows[0].Signals, rows[0].PicturesUsed)
	}
	if rows[1].Signals["meme"] != 0.7 || rows[1].PicturesUsed != 1 || rows[1].Model != "v5" {
		t.Errorf("picture post: %v, pictures used %d, model %q", rows[1].Signals, rows[1].PicturesUsed, rows[1].Model)
	}
	if _, has := rows[2].Signals["meme"]; has || rows[2].PicturesUsed != 0 {
		t.Errorf("post whose picture could not be read: meme stored %v, pictures used %d", rows[2].Signals, rows[2].PicturesUsed)
	}
	if rows[0].Signals["news"] != 0.1 {
		t.Errorf("other signals are kept: %v", rows[0].Signals)
	}
}

func TestClassifyRejectsAMismatchedAnswer(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"v5","results":[]}`))
	}))
	defer srv.Close()
	c := &Classifier{URL: srv.URL, Client: srv.Client()}
	if _, _, err := c.Classify(context.Background(), []Item{{Text: "x"}}); err == nil || !strings.Contains(err.Error(), "0 results for 1 posts") {
		t.Errorf("want a count error, got %v", err)
	}
}

func TestClassifyReportsServiceErrors(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusBadRequest)
		_, _ = w.Write([]byte(`{"error":"pictures must be base64"}`))
	}))
	defer srv.Close()
	c := &Classifier{URL: srv.URL, Client: srv.Client()}
	if _, _, err := c.Classify(context.Background(), []Item{{Text: "x"}}); err == nil || !strings.Contains(err.Error(), "pictures must be base64") {
		t.Errorf("want the service's message, got %v", err)
	}
}

func TestHealthReadsMaxImages(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"model":"v5","taxonomy_version":"v2.1","postdoc_version":"pd2","device":"cuda","max_images":2,"signals":["meme"]}`))
	}))
	defer srv.Close()
	c := &Classifier{URL: srv.URL, Client: srv.Client()}
	h, err := c.Health(context.Background())
	if err != nil || h.Model != "v5" || h.MaxImages != 2 || h.PostdocVersion != "pd2" || len(h.Signals) != 1 {
		t.Errorf("health %+v %v", h, err)
	}
}

func TestFetchLimitsSizeAndChecksStatus(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/missing" {
			http.NotFound(w, r)
			return
		}
		_, _ = w.Write([]byte(strings.Repeat("x", 100)))
	}))
	defer srv.Close()
	if b, err := Fetch(context.Background(), srv.Client(), srv.URL+"/ok", 10); err != nil || len(b) != 10 {
		t.Errorf("size limit: %d bytes, %v", len(b), err)
	}
	if _, err := Fetch(context.Background(), srv.Client(), srv.URL+"/missing", 10); err == nil {
		t.Error("a 404 should be an error")
	}
}
