// Package postmap serves the post map: every post on a 2D map, grouped into segments and areas that an LLM has
// named, built offline by postclusters/build_snapshot.py as immutable snapshot directories. It is read-only,
// keeps no state beyond the newest snapshot, and does not touch the database.
package postmap

import (
	"bufio"
	"compress/gzip"
	"embed"
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
)

//go:embed web
var webFS embed.FS

var (
	snapID = regexp.MustCompile(`^[0-9]{8}T[0-9]{6}Z$`)
	// The snapshot files the browser may fetch, and their content types. posts.jsonl and texts.txt are far too big
	// to hand out whole; posts are fetched one at a time through the API.
	snapFiles = map[string]string{
		"map.json":     "application/json",
		"xy.f32":       "application/octet-stream",
		"seg.u16":      "application/octet-stream",
		"outraged.u8":  "application/octet-stream",
		"sentiment.u8": "application/octet-stream",
		"critical.u8":  "application/octet-stream",
	}
)

const (
	maxIDs         = 60
	maxSearch      = 100
	minSearchChars = 3
)

// Snapshot is one built map, loaded for serving.
type Snapshot struct {
	ID   string
	Dir  string
	Meta struct {
		BuiltAt int64 `json:"built_at"`
		Posts   int   `json:"n_posts"`
		Total   int   `json:"n_posts_total"`
		Segs    int   `json:"n_segments"`
		Areas   int   `json:"n_areas"`
		From    int64 `json:"from"`
		To      int64 `json:"to"`
	}
	Segments []json.RawMessage // map.json's segments, by id
	Areas    []json.RawMessage
	texts    []string // lower case, for search
	seg      []uint16 // each post's segment
	offsets  []uint64
	posts    *os.File
}

// Server serves the newest snapshot under Dir.
type Server struct {
	Dir string
	Log *slog.Logger
	// Limits, when set, keep clients from asking too often; set them when the service can be reached from the internet.
	Limits *Limits

	mu   sync.RWMutex
	snap *Snapshot
}

// Load reads the snapshot that Dir/latest points at, if it is not the one already loaded.
func (s *Server) Load() error {
	target, err := os.Readlink(filepath.Join(s.Dir, "latest"))
	if err != nil {
		return fmt.Errorf("no map yet in %s: %w", s.Dir, err)
	}
	id := filepath.Base(target)
	s.mu.RLock()
	cur := s.snap
	s.mu.RUnlock()
	if cur != nil && cur.ID == id {
		return nil
	}
	if !snapID.MatchString(id) {
		return fmt.Errorf("latest points at %q, which is not a snapshot id", target)
	}
	start := time.Now()
	snap, err := loadSnapshot(filepath.Join(s.Dir, id), id)
	if err != nil {
		return err
	}
	s.mu.Lock()
	old := s.snap
	s.snap = snap
	s.mu.Unlock()
	if old != nil {
		old.posts.Close()
	}
	s.Log.Info("loaded map", "id", id, "posts", snap.Meta.Posts, "segments", snap.Meta.Segs, "took", time.Since(start).String())
	return nil
}

// Watch reloads when a new snapshot appears, until stop is closed.
func (s *Server) Watch(every time.Duration, stop <-chan struct{}) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-stop:
			return
		case <-t.C:
			if err := s.Load(); err != nil {
				s.Log.Warn("reload failed", "err", err)
			}
		}
	}
}

func loadSnapshot(dir, id string) (*Snapshot, error) {
	sn := &Snapshot{ID: id, Dir: dir}
	raw, err := os.ReadFile(filepath.Join(dir, "map.json"))
	if err != nil {
		return nil, err
	}
	var m struct {
		Segments []json.RawMessage `json:"segments"`
		Areas    []json.RawMessage `json:"areas"`
	}
	if err := json.Unmarshal(raw, &m); err != nil {
		return nil, fmt.Errorf("map.json: %w", err)
	}
	if err := json.Unmarshal(raw, &sn.Meta); err != nil {
		return nil, fmt.Errorf("map.json: %w", err)
	}
	sn.Segments, sn.Areas = m.Segments, m.Areas

	offRaw, err := os.ReadFile(filepath.Join(dir, "posts.off"))
	if err != nil {
		return nil, err
	}
	sn.offsets = make([]uint64, len(offRaw)/8)
	for i := range sn.offsets {
		sn.offsets[i] = binary.LittleEndian.Uint64(offRaw[i*8:])
	}
	n := len(sn.offsets) - 1
	if n != sn.Meta.Posts {
		return nil, fmt.Errorf("posts.off has %d posts, map.json says %d", n, sn.Meta.Posts)
	}
	f, err := os.Open(filepath.Join(dir, "texts.txt"))
	if err != nil {
		return nil, err
	}
	defer f.Close()
	sn.texts = make([]string, 0, n)
	sc := bufio.NewScanner(f)
	sc.Buffer(make([]byte, 1<<20), 1<<20)
	for sc.Scan() {
		sn.texts = append(sn.texts, sc.Text())
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	if len(sn.texts) != n {
		return nil, fmt.Errorf("texts.txt has %d lines for %d posts", len(sn.texts), n)
	}
	segRaw, err := os.ReadFile(filepath.Join(dir, "seg.u16"))
	if err != nil {
		return nil, err
	}
	if len(segRaw) != 2*n {
		return nil, fmt.Errorf("seg.u16 has %d entries for %d posts", len(segRaw)/2, n)
	}
	sn.seg = make([]uint16, n)
	for i := range sn.seg {
		sn.seg[i] = binary.LittleEndian.Uint16(segRaw[2*i:])
	}
	if sn.posts, err = os.Open(filepath.Join(dir, "posts.jsonl")); err != nil {
		return nil, err
	}
	return sn, nil
}

var errNotFound = errors.New("not found")

// post returns the raw JSON line of post i.
func (sn *Snapshot) post(i int) (json.RawMessage, error) {
	if i < 0 || i >= len(sn.texts) {
		return nil, errNotFound
	}
	buf := make([]byte, sn.offsets[i+1]-sn.offsets[i])
	if _, err := sn.posts.ReadAt(buf, int64(sn.offsets[i])); err != nil && !errors.Is(err, io.EOF) {
		return nil, err
	}
	return json.RawMessage(strings.TrimRight(string(buf), "\n")), nil
}

// search returns the indices of up to limit posts whose text contains q (lower case), in index order, optionally
// only those in a segment (when filter is true; a segment number no post has matches nothing). The scan is split over a few goroutines, each stopping at limit.
func (sn *Snapshot) search(q string, limit int, filter bool, segment int) []int {
	const parts = 16
	n := len(sn.texts)
	size := (n + parts - 1) / parts
	res := make([][]int, parts)
	var wg sync.WaitGroup
	for p := 0; p < parts; p++ {
		wg.Add(1)
		go func(p int) {
			defer wg.Done()
			lo, hi := p*size, min((p+1)*size, n)
			for i := lo; i < hi && len(res[p]) < limit; i++ {
				if strings.Contains(sn.texts[i], q) && (!filter || int(sn.seg[i]) == segment) {
					res[p] = append(res[p], i)
				}
			}
		}(p)
	}
	wg.Wait()
	var out []int
	for _, r := range res {
		out = append(out, r...)
		if len(out) >= limit {
			return out[:limit]
		}
	}
	return out
}

// Handler returns the routes.
func (s *Server) Handler() http.Handler {
	mux := http.NewServeMux()
	web, _ := fs.Sub(webFS, "web")
	mux.Handle("GET /", revalidate(http.FileServerFS(web)))
	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		if s.current() == nil {
			http.Error(w, "no map loaded", http.StatusServiceUnavailable)
			return
		}
		io.WriteString(w, "ok\n")
	})
	mux.HandleFunc("GET /api/latest", s.handleLatest)
	mux.HandleFunc("GET /snap/{id}/{name}", s.handleSnapFile)
	posts := func(h http.HandlerFunc) http.HandlerFunc {
		if s.Limits == nil {
			return h
		}
		return s.Limits.limit("posts", s.Limits.PostsPerSec, s.Limits.PostsBurst, h)
	}
	search := s.handleSearch
	if s.Limits != nil {
		search = s.Limits.limit("search", s.Limits.SearchPerSec, s.Limits.SearchBurst, s.handleSearch)
	}
	mux.HandleFunc("GET /api/v1/overview", posts(s.handleOverview))
	mux.HandleFunc("GET /api/v1/segments/{id}", posts(s.handleSegment))
	mux.HandleFunc("GET /api/v1/post/{i}", posts(s.handlePost))
	mux.HandleFunc("GET /api/v1/posts", posts(s.handlePosts))
	mux.HandleFunc("GET /api/v1/search", search)
	mux.HandleFunc("GET /robots.txt", func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/plain; charset=utf-8")
		io.WriteString(w, "User-agent: *\nDisallow: /api/\nDisallow: /snap/\n")
	})
	return protect(mux)
}

func (s *Server) current() *Snapshot {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.snap
}

// revalidate makes browsers ask whether the site's own files changed before reusing them.
func revalidate(h http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Cache-Control", "no-cache")
		h.ServeHTTP(w, r)
	})
}

func writeJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(v)
}

func (s *Server) snapOr503(w http.ResponseWriter) *Snapshot {
	sn := s.current()
	if sn == nil {
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "no map has been built yet"})
	}
	return sn
}

func (s *Server) handleLatest(w http.ResponseWriter, r *http.Request) {
	if sn := s.snapOr503(w); sn != nil {
		w.Header().Set("Cache-Control", "no-cache")
		w.Header().Set("Content-Type", "application/json")
		json.NewEncoder(w).Encode(map[string]any{"id": sn.ID, "built_at": sn.Meta.BuiltAt, "posts": sn.Meta.Posts})
	}
}

// handleSnapFile serves one file of a snapshot. Snapshots never change, so browsers keep them.
func (s *Server) handleSnapFile(w http.ResponseWriter, r *http.Request) {
	id, name := r.PathValue("id"), r.PathValue("name")
	ctype, ok := snapFiles[name]
	if !ok || !snapID.MatchString(id) {
		http.NotFound(w, r)
		return
	}
	f, err := os.Open(filepath.Join(s.Dir, id, name))
	if err != nil {
		http.NotFound(w, r)
		return
	}
	defer f.Close()
	st, err := f.Stat()
	if err != nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", ctype)
	w.Header().Set("Cache-Control", "public, max-age=31536000, immutable")
	if strings.HasSuffix(name, ".json") && strings.Contains(r.Header.Get("Accept-Encoding"), "gzip") {
		w.Header().Set("Content-Encoding", "gzip")
		w.Header().Set("Vary", "Accept-Encoding")
		gz, _ := gzip.NewWriterLevel(w, 6)
		io.Copy(gz, f)
		gz.Close()
		return
	}
	http.ServeContent(w, r, name, st.ModTime(), f)
}

func (s *Server) handleOverview(w http.ResponseWriter, r *http.Request) {
	if sn := s.snapOr503(w); sn != nil {
		writeJSON(w, 200, map[string]any{"snapshot": sn.ID, "meta": sn.Meta, "areas": sn.Areas})
	}
}

func (s *Server) handleSegment(w http.ResponseWriter, r *http.Request) {
	sn := s.snapOr503(w)
	if sn == nil {
		return
	}
	id, err := strconv.Atoi(r.PathValue("id"))
	if err != nil || id < 0 || id >= len(sn.Segments) {
		writeJSON(w, 404, map[string]string{"error": "no such segment"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Write(sn.Segments[id])
}

func (s *Server) handlePost(w http.ResponseWriter, r *http.Request) {
	sn := s.snapOr503(w)
	if sn == nil {
		return
	}
	i, err := strconv.Atoi(r.PathValue("i"))
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "no such post"})
		return
	}
	rec, err := sn.post(i)
	if err != nil {
		writeJSON(w, 404, map[string]string{"error": "no such post"})
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=60")
	w.Write(rec)
}

// handlePosts returns several posts by index (?ids=1,2,3), in the order asked, skipping any that do not exist.
func (s *Server) handlePosts(w http.ResponseWriter, r *http.Request) {
	sn := s.snapOr503(w)
	if sn == nil {
		return
	}
	var out []json.RawMessage
	for _, part := range strings.Split(r.URL.Query().Get("ids"), ",") {
		if len(out) == maxIDs {
			break
		}
		i, err := strconv.Atoi(strings.TrimSpace(part))
		if err != nil {
			continue
		}
		if rec, err := sn.post(i); err == nil {
			out = append(out, rec)
		}
	}
	if out == nil {
		out = []json.RawMessage{}
	}
	writeJSON(w, 200, map[string]any{"snapshot": sn.ID, "posts": out})
}

// handleSearch finds posts whose text contains q (case-insensitive, at least minSearchChars characters),
// optionally within one segment (?segment=12).
func (s *Server) handleSearch(w http.ResponseWriter, r *http.Request) {
	sn := s.snapOr503(w)
	if sn == nil {
		return
	}
	q := strings.ToLower(strings.Join(strings.Fields(r.URL.Query().Get("q")), " "))
	if s.Limits != nil && s.Limits.MaxQueryChars > 0 && len([]rune(q)) > s.Limits.MaxQueryChars {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": fmt.Sprintf("search text is limited to %d characters", s.Limits.MaxQueryChars)})
		return
	}
	if len([]rune(q)) < minSearchChars {
		writeJSON(w, 200, map[string]any{"snapshot": sn.ID, "posts": []json.RawMessage{}, "note": fmt.Sprintf("type at least %d characters", minSearchChars)})
		return
	}
	done := s.Limits.startSearch()
	if done == nil {
		w.Header().Set("Retry-After", "1")
		writeJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "busy: try again in a moment"})
		return
	}
	defer done()
	limit, _ := strconv.Atoi(r.URL.Query().Get("limit"))
	if limit <= 0 || limit > maxSearch {
		limit = 30
	}
	filter, segment := false, 0
	if v := r.URL.Query().Get("segment"); v != "" {
		filter = true
		var err error
		if segment, err = strconv.Atoi(v); err != nil || segment < 0 {
			segment = -1 // no post is in this segment, so nothing matches
		}
	}
	ids := sn.search(q, limit, filter, segment)
	sort.Ints(ids)
	out := make([]json.RawMessage, 0, len(ids))
	for _, i := range ids {
		if rec, err := sn.post(i); err == nil {
			out = append(out, rec)
		}
	}
	writeJSON(w, 200, map[string]any{"snapshot": sn.ID, "posts": out})
}
