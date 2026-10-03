package imagearchive

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"image"
	"image/color"
	"image/jpeg"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"golang.org/x/time/rate"
)

// jpegBytes makes a small valid JPEG with the given pixel size.
func jpegBytes(t *testing.T, w, h int) []byte {
	t.Helper()
	img := image.NewRGBA(image.Rect(0, 0, w, h))
	for x := 0; x < w; x++ {
		for y := 0; y < h; y++ {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 128, A: 255})
		}
	}
	var buf bytesBuffer
	if err := jpeg.Encode(&buf, img, nil); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// newFetchTestServer answers every download with fn; it returns the base URL of the
// test server so tests fetch real, routable URLs.
func newFetchTestServer(t *testing.T, fn func(w http.ResponseWriter, r *http.Request)) string {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(fn))
	t.Cleanup(srv.Close)
	return srv.URL
}

// fetchRow runs fetchOne for one row and returns the updated row.
func fetchRow(t *testing.T, base, root, path string) ImageRow {
	t.Helper()
	f := &Fetcher{Root: root}
	row := ImageRow{URI: "at://u/1", Idx: 0, Kind: KindImage, URL: base + path, Status: StatusPending}
	got, err := f.fetchOne(context.Background(), http.DefaultClient, unlimitedLimiter(), row, testNow())
	if err != nil {
		t.Fatalf("fetchOne: %v", err)
	}
	return got
}

func TestFetchOK(t *testing.T) {
	body := jpegBytes(t, 320, 240)
	base := newFetchTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("User-Agent") != UserAgent {
			t.Errorf("User-Agent = %q", r.Header.Get("User-Agent"))
		}
		w.Write(body)
	})
	root := t.TempDir()
	row := fetchRow(t, base, root, "/pic.jpg")
	if row.Status != StatusOK {
		t.Fatalf("status = %q, want ok: %+v", row.Status, row)
	}
	sha := hex.EncodeToString(sha256Sum(body))
	if row.Sha256 != sha {
		t.Fatalf("sha256 = %q, want %q", row.Sha256, sha)
	}
	if row.Width != 320 || row.Height != 240 || row.Bytes != uint32(len(body)) {
		t.Fatalf("row = %+v", row)
	}
	if _, err := os.Stat(filepath.Join(root, "raw", sha[:2], sha+".jpg")); err != nil {
		t.Fatalf("file missing: %v", err)
	}
}

func TestFetch404Gone(t *testing.T) {
	base := newFetchTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "not found", http.StatusNotFound)
	})
	row := fetchRow(t, base, t.TempDir(), "/pic.jpg")
	if row.Status != StatusImgGone {
		t.Fatalf("status = %q, want gone: %+v", row.Status, row)
	}
}

func TestFetchBadImage(t *testing.T) {
	base := newFetchTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write([]byte("this is not a picture at all"))
	})
	row := fetchRow(t, base, t.TempDir(), "/pic.jpg")
	if row.Status != StatusBadImage {
		t.Fatalf("status = %q, want bad_image: %+v", row.Status, row)
	}
}

func TestFetch503Then200(t *testing.T) {
	body := jpegBytes(t, 64, 64)
	var served int32
	base := newFetchTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		if atomic.AddInt32(&served, 1) == 1 {
			http.Error(w, "try later", http.StatusServiceUnavailable)
			return
		}
		w.Write(body)
	})
	f := &Fetcher{Root: t.TempDir()}
	row := ImageRow{URI: "at://u/1", URL: base + "/pic.jpg", Status: StatusPending}
	got, err := f.fetchOne(context.Background(), http.DefaultClient, unlimitedLimiter(), row, testNow())
	if err != nil {
		t.Fatal(err)
	}
	if got.Status != StatusOK {
		t.Fatalf("status = %q, want ok after retry: %+v", got.Status, got)
	}
	if got.Attempts != 2 {
		t.Fatalf("attempts = %d, want 2", got.Attempts)
	}
}

func TestFetchDedupe(t *testing.T) {
	// The same picture at two URLs: two rows, one file.
	body := jpegBytes(t, 100, 100)
	base := newFetchTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		w.Write(body)
	})
	root := t.TempDir()
	for _, path := range []string{"/a.jpg", "/b.jpg"} {
		row := fetchRow(t, base, root, path)
		if row.Status != StatusOK {
			t.Fatalf("status = %q: %+v", row.Status, row)
		}
	}
	sha := hex.EncodeToString(sha256Sum(body))
	dir := filepath.Join(root, "raw", sha[:2])
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	n := 0
	for _, e := range entries {
		if len(e.Name()) >= len(sha) && e.Name()[:len(sha)] == sha {
			n++
		}
	}
	if n != 1 {
		t.Fatalf("found %d files for sha %s, want exactly 1", n, sha)
	}
}

func TestFetchErrorAfterRetries(t *testing.T) {
	base := newFetchTestServer(t, func(w http.ResponseWriter, r *http.Request) {
		http.Error(w, "down", http.StatusServiceUnavailable)
	})
	row := fetchRow(t, base, t.TempDir(), "/pic.jpg")
	if row.Status != StatusErrorRow {
		t.Fatalf("status = %q, want error: %+v", row.Status, row)
	}
	if row.Attempts != MaxAttempts {
		t.Fatalf("attempts = %d, want %d", row.Attempts, MaxAttempts)
	}
}

// helpers

// unlimitedLimiter takes no time between downloads, so tests are fast.
func unlimitedLimiter() *rate.Limiter { return rate.NewLimiter(rate.Inf, 1) }

type bytesBuffer struct{ b []byte }

func (bb *bytesBuffer) Write(p []byte) (int, error) { bb.b = append(bb.b, p...); return len(p), nil }
func (bb *bytesBuffer) Bytes() []byte               { return bb.b }

func sha256Sum(b []byte) []byte {
	s := sha256.Sum256(b)
	return s[:]
}

func testNow() time.Time { return time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC) }
