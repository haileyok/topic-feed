package postmap

import (
	"encoding/binary"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

const testSnap = "20261009T072608Z"

var testPosts = []struct {
	text string
	seg  int
}{
	{"The Quick Brown Fox", 0},
	{"quick   thinking about AI art", 1},
	{"A slow day, nothing happens", 1},
	{"Another QUICK one in segment two", 2},
}

// writeSnapshot lays out a small snapshot the way postclusters/build_snapshot.py does.
func writeSnapshot(t *testing.T, root, id string) {
	t.Helper()
	dir := filepath.Join(root, id)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	write := func(name string, b []byte) {
		if err := os.WriteFile(filepath.Join(dir, name), b, 0o644); err != nil {
			t.Fatal(err)
		}
	}
	segs := []map[string]any{
		{"id": 0, "title": "Foxes", "area": 0, "n_texts": 1},
		{"id": 1, "title": "Quick things", "area": 0, "n_texts": 2},
		{"id": 2, "title": "Other", "area": 1, "n_texts": 1},
	}
	meta, _ := json.Marshal(map[string]any{
		"id": id, "built_at": 1, "n_posts": len(testPosts), "n_posts_total": 9, "n_segments": 3, "n_areas": 2,
		"from": 10, "to": 20, "areas": []map[string]any{{"id": 0, "title": "A"}, {"id": 1, "title": "B"}}, "segments": segs,
	})
	write("map.json", meta)
	var jsonl, texts strings.Builder
	offs := make([]byte, 8*(len(testPosts)+1))
	segB := make([]byte, 2*len(testPosts))
	for i, p := range testPosts {
		binary.LittleEndian.PutUint64(offs[8*i:], uint64(jsonl.Len()))
		rec, _ := json.Marshal(map[string]any{"i": i, "u": "at://did:plc:x/app.bsky.feed.post/r" + string(rune('a'+i)), "t": p.text, "n": 1, "a": 1, "at": 10, "p": "x/y", "s": p.seg})
		jsonl.Write(rec)
		jsonl.WriteString("\n")
		texts.WriteString(strings.ToLower(strings.Join(strings.Fields(p.text), " ")) + "\n")
		binary.LittleEndian.PutUint16(segB[2*i:], uint16(p.seg))
	}
	binary.LittleEndian.PutUint64(offs[8*len(testPosts):], uint64(jsonl.Len()))
	write("posts.jsonl", []byte(jsonl.String()))
	write("texts.txt", []byte(texts.String()))
	write("posts.off", offs)
	write("seg.u16", segB)
	for _, f := range []string{"xy.f32", "outraged.u8", "sentiment.u8", "critical.u8"} {
		write(f, []byte("xxxxxxxx"))
	}
}

func newTestServer(t *testing.T) (*Server, *httptest.Server) {
	t.Helper()
	root := t.TempDir()
	writeSnapshot(t, root, testSnap)
	if err := os.Symlink(testSnap, filepath.Join(root, "latest")); err != nil {
		t.Fatal(err)
	}
	s := &Server{Dir: root, Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	ts := httptest.NewServer(s.Handler())
	t.Cleanup(ts.Close)
	return s, ts
}

func get(t *testing.T, url string) (int, []byte, http.Header) {
	t.Helper()
	resp, err := http.Get(url)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(resp.Body)
	return resp.StatusCode, b, resp.Header
}

type postsResp struct {
	Posts []struct {
		I int    `json:"i"`
		T string `json:"t"`
		S int    `json:"s"`
	} `json:"posts"`
	Note string `json:"note"`
}

func TestPostByIndex(t *testing.T) {
	t.Parallel()
	_, ts := newTestServer(t)
	code, body, _ := get(t, ts.URL+"/api/v1/post/1")
	if code != 200 {
		t.Fatalf("status %d: %s", code, body)
	}
	var p struct {
		I int    `json:"i"`
		T string `json:"t"`
	}
	if err := json.Unmarshal(body, &p); err != nil || p.I != 1 || p.T != "quick   thinking about AI art" {
		t.Errorf("got %s (%v)", body, err)
	}
	for _, bad := range []string{"4", "-1", "x", "99999999999999999999"} {
		if code, _, _ := get(t, ts.URL+"/api/v1/post/"+bad); code != 404 {
			t.Errorf("post %s: status %d, want 404", bad, code)
		}
	}
}

func TestSeveralPostsInTheOrderAskedSkippingBadOnes(t *testing.T) {
	t.Parallel()
	_, ts := newTestServer(t)
	_, body, _ := get(t, ts.URL+"/api/v1/posts?ids=3,0,nope,9,2")
	var r postsResp
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	var got []int
	for _, p := range r.Posts {
		got = append(got, p.I)
	}
	if len(got) != 3 || got[0] != 3 || got[1] != 0 || got[2] != 2 {
		t.Errorf("got %v, want [3 0 2]", got)
	}
	_, body, _ = get(t, ts.URL+"/api/v1/posts?ids=")
	if !strings.Contains(string(body), `"posts":[]`) {
		t.Errorf("no ids should give an empty list, got %s", body)
	}
	// At most maxIDs posts come back however many are asked for.
	var ids []string
	for i := 0; i < 500; i++ {
		ids = append(ids, "0")
	}
	_, body, _ = get(t, ts.URL+"/api/v1/posts?ids="+strings.Join(ids, ","))
	_ = json.Unmarshal(body, &r)
	if len(r.Posts) != maxIDs {
		t.Errorf("asked for 500, got %d, want %d", len(r.Posts), maxIDs)
	}
}

func TestSearchIsCaseAndSpaceInsensitiveAndCanBeLimitedToASegment(t *testing.T) {
	t.Parallel()
	_, ts := newTestServer(t)
	search := func(q string) postsResp {
		_, body, _ := get(t, ts.URL+"/api/v1/search?q="+q)
		var r postsResp
		if err := json.Unmarshal(body, &r); err != nil {
			t.Fatal(err)
		}
		return r
	}
	r := search("QUICK")
	if len(r.Posts) != 3 || r.Posts[0].I != 0 || r.Posts[1].I != 1 || r.Posts[2].I != 3 {
		t.Errorf("quick: %+v", r.Posts)
	}
	// Runs of whitespace in the post or in the query don't matter.
	if r := search("quick%20%20%20thinking"); len(r.Posts) != 1 || r.Posts[0].I != 1 {
		t.Errorf("spaces: %+v", r.Posts)
	}
	if r := search("ai%20art"); len(r.Posts) != 1 {
		t.Errorf("ai art: %+v", r.Posts)
	}
	_, body, _ := get(t, ts.URL+"/api/v1/search?q=quick&segment=1")
	var seg postsResp
	json.Unmarshal(body, &seg)
	if len(seg.Posts) != 1 || seg.Posts[0].S != 1 {
		t.Errorf("segment 1: %+v", seg.Posts)
	}
	_, body, _ = get(t, ts.URL+"/api/v1/search?q=quick&segment=-5")
	json.Unmarshal(body, &seg)
	if len(seg.Posts) != 0 {
		t.Errorf("a negative segment should match nothing: %+v", seg.Posts)
	}
	_, body, _ = get(t, ts.URL+"/api/v1/search?q=quick&limit=2")
	json.Unmarshal(body, &seg)
	if len(seg.Posts) != 2 {
		t.Errorf("limit 2: %d posts", len(seg.Posts))
	}
	if r := search("qu"); len(r.Posts) != 0 || r.Note == "" {
		t.Errorf("a two-character query should be refused with a note: %+v", r)
	}
	if r := search("zzzzzz"); len(r.Posts) != 0 {
		t.Errorf("no match: %+v", r.Posts)
	}
}

func TestSegmentsAndOverview(t *testing.T) {
	t.Parallel()
	_, ts := newTestServer(t)
	code, body, _ := get(t, ts.URL+"/api/v1/segments/1")
	if code != 200 || !strings.Contains(string(body), "Quick things") {
		t.Errorf("segment 1: %d %s", code, body)
	}
	for _, bad := range []string{"3", "-1", "x"} {
		if code, _, _ := get(t, ts.URL+"/api/v1/segments/"+bad); code != 404 {
			t.Errorf("segment %s: %d, want 404", bad, code)
		}
	}
	code, body, _ = get(t, ts.URL+"/api/v1/overview")
	if code != 200 || !strings.Contains(string(body), `"n_segments":3`) {
		t.Errorf("overview: %d %s", code, body)
	}
}

func TestOnlyTheMapFilesAreServedAndNothingOutsideTheSnapshot(t *testing.T) {
	t.Parallel()
	_, ts := newTestServer(t)
	for _, name := range []string{"map.json", "xy.f32", "seg.u16", "outraged.u8", "sentiment.u8", "critical.u8"} {
		code, _, h := get(t, ts.URL+"/snap/"+testSnap+"/"+name)
		if code != 200 || !strings.Contains(h.Get("Cache-Control"), "immutable") {
			t.Errorf("%s: %d %q", name, code, h.Get("Cache-Control"))
		}
	}
	for _, p := range []string{
		"/snap/" + testSnap + "/posts.jsonl", // the text of every post: only through the API, a few at a time
		"/snap/" + testSnap + "/texts.txt",
		"/snap/" + testSnap + "/posts.off",
		"/snap/" + testSnap + "/..%2F..%2Fetc%2Fpasswd",
		"/snap/..%2F" + testSnap + "/map.json",
		"/snap/latest/map.json",
	} {
		if code, _, _ := get(t, ts.URL+p); code == 200 {
			t.Errorf("%s was served", p)
		}
	}
}

func TestNoMapYetIs503ButTheSiteStillServes(t *testing.T) {
	t.Parallel()
	s := &Server{Dir: t.TempDir(), Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
	if err := s.Load(); err == nil {
		t.Fatal("Load with no snapshot should fail")
	}
	ts := httptest.NewServer(s.Handler())
	defer ts.Close()
	for _, p := range []string{"/api/latest", "/api/v1/overview", "/api/v1/post/0", "/api/v1/posts?ids=0", "/api/v1/search?q=quick", "/healthz"} {
		if code, _, _ := get(t, ts.URL+p); code != 503 {
			t.Errorf("%s: %d, want 503", p, code)
		}
	}
	if code, _, _ := get(t, ts.URL+"/"); code != 200 {
		t.Errorf("index: %d", code)
	}
}

func TestReloadPicksUpANewSnapshotAndRefusesBadOnes(t *testing.T) {
	t.Parallel()
	s, _ := newTestServer(t)
	newID := "20261010T000000Z"
	writeSnapshot(t, s.Dir, newID)
	os.Remove(filepath.Join(s.Dir, "latest"))
	if err := os.Symlink(newID, filepath.Join(s.Dir, "latest")); err != nil {
		t.Fatal(err)
	}
	if err := s.Load(); err != nil {
		t.Fatal(err)
	}
	if got := s.current().ID; got != newID {
		t.Errorf("serving %s, want %s", got, newID)
	}
	os.Remove(filepath.Join(s.Dir, "latest"))
	os.Symlink("../../etc", filepath.Join(s.Dir, "latest"))
	if err := s.Load(); err == nil {
		t.Error("a latest link to a non-snapshot should be refused")
	}
	if got := s.current().ID; got != newID {
		t.Errorf("after a refused reload serving %s, want %s", got, newID)
	}
}

func TestASnapshotWhoseFilesDisagreeIsRefused(t *testing.T) {
	t.Parallel()
	root := t.TempDir()
	writeSnapshot(t, root, testSnap)
	// one text line too few for the posts
	texts, _ := os.ReadFile(filepath.Join(root, testSnap, "texts.txt"))
	lines := strings.Split(strings.TrimRight(string(texts), "\n"), "\n")
	os.WriteFile(filepath.Join(root, testSnap, "texts.txt"), []byte(strings.Join(lines[:len(lines)-1], "\n")+"\n"), 0o644)
	if _, err := loadSnapshot(filepath.Join(root, testSnap), testSnap); err == nil {
		t.Error("texts.txt with too few lines should be refused")
	}
	root2 := t.TempDir()
	writeSnapshot(t, root2, testSnap)
	os.WriteFile(filepath.Join(root2, testSnap, "seg.u16"), []byte{0, 0}, 0o644)
	if _, err := loadSnapshot(filepath.Join(root2, testSnap), testSnap); err == nil {
		t.Error("seg.u16 with too few entries should be refused")
	}
}
