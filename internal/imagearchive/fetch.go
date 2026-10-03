package imagearchive

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"image"
	"io"
	"log/slog"
	"net/http"
	"os"
	"path/filepath"
	"strconv"
	"sync"
	"time"

	_ "image/gif"
	_ "image/jpeg"
	_ "image/png"

	"golang.org/x/time/rate"

	"github.com/haileyok/topic-feed/internal/chdb"
)

// UserAgent is what the archive tells the CDN it is.
const UserAgent = "topic-feed-image-archive/1.0"

// maxBytes is the most the archive reads of one picture (40 MiB).
const maxBytes = 40 << 20

// goneCodes are HTTP answers that mean the picture is not there anymore.
var goneCodes = map[int]bool{403: true, 404: true, 410: true, 451: true}

// Fetcher downloads the archive's pending pictures into a content-addressed directory
// tree: <root>/raw/<sha256[:2]>/<sha256>.<ext>. A picture posted by two posts is stored
// once; reruns skip what is already on disk and what is already settled in ClickHouse.
type Fetcher struct {
	Store   *Store
	Root    string       // archive root; raw files under <root>/raw
	Client  *http.Client // default: 30 second timeout
	Log     *slog.Logger
	Workers int           // parallel downloads; default 72
	RPS     float64       // downloads per second, shared by the workers; default 60
	Batch   int           // rows inserted per write; default 500
	Limiter *rate.Limiter // shared; default RPS
	Now     func() time.Time
}

// Run fetches pictures until none are pending, then makes one more pass over rows that
// ended in error while they still have attempts left. limit caps the pending rows
// fetched (0 for all); -limit is not allowed, use 0.
func (f *Fetcher) Run(ctx context.Context, limit int) error {
	if err := f.runPass(ctx, []string{StatusPending}, limit); err != nil {
		return err
	}
	return f.runPass(ctx, []string{StatusErrorRow}, 0)
}

// runPass fetches rows with one of statuses, up to limit (0 for all).
func (f *Fetcher) runPass(ctx context.Context, statuses []string, limit int) error {
	workers := f.Workers
	if workers <= 0 {
		workers = 24
	}
	rps := f.RPS
	if rps <= 0 {
		rps = 60
	}
	limiter := f.Limiter
	if limiter == nil {
		limiter = rate.NewLimiter(rate.Limit(rps), int(rps))
	}
	batch := f.Batch
	if batch <= 0 {
		batch = 500
	}
	now := f.Now
	if now == nil {
		now = time.Now
	}
	log := f.Log
	if log == nil {
		log = slog.Default()
	}
	client := f.Client
	if client == nil {
		client = &http.Client{Timeout: 60 * time.Second}
	}

	var done int
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		take := batch * 10 // rows read per outer loop
		if limit > 0 {
			take = min(take, limit-done)
		}
		rows, err := f.Store.PendingImages(ctx, statuses, take)
		if err != nil {
			return err
		}
		if len(rows) == 0 {
			log.Info("fetch pass done", "statuses", statuses, "fetched", done)
			return nil
		}

		// A picture already settled ok at another row needs no download: the same
		// file is already on disk. Settle it from the file's facts.
		results := make([]ImageRow, len(rows))
		var wg sync.WaitGroup
		var next int64 = 0
		var mu sync.Mutex
		var werr error
		for w := 0; w < workers; w++ {
			wg.Add(1)
			go func() {
				defer wg.Done()
				for {
					mu.Lock()
					i := int(next)
					if i >= len(rows) {
						mu.Unlock()
						return
					}
					next++
					mu.Unlock()
					if ctx.Err() != nil {
						return
					}
					row, err := f.fetchOne(ctx, client, limiter, rows[i], now())
					mu.Lock()
					results[i] = row
					if err != nil {
						werr = errors.Join(werr, err)
					}
					mu.Unlock()
				}
			}()
		}
		wg.Wait()
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if werr != nil {
			return fmt.Errorf("fetching: %w", werr)
		}
		if err := chdb.Insert(ctx, f.Store.Conn, "post_images", results); err != nil {
			return err
		}
		done += len(rows)
		log.Info("fetch progress", "statuses", statuses, "done", done, "rows", len(rows))
		if limit > 0 && done >= limit {
			log.Info("fetch reached limit", "done", done)
			return nil
		}
	}
}

// fetchOne downloads one picture and returns the row to write for it. Only a context
// error or a disk failure is an error; everything else is settled in the row's status.
func (f *Fetcher) fetchOne(ctx context.Context, client *http.Client, limiter *rate.Limiter, row ImageRow, now time.Time) (ImageRow, error) {
	row.FetchedAt = now
	row.UpdatedAt = now
	row.Error = ""
	wait := time.Second // first backoff between fetch attempts of one picture
	for attempt := 0; ; attempt++ {
		row.Attempts = uint8(attempt + 1) // fetches tried so far, this one included
		if err := limiter.Wait(ctx); err != nil {
			return row, err
		}
		body, w, h, format, err := f.download(ctx, client, row.URL)
		if err == nil {
			// The bytes name the file; decoding them told us they are a picture.
			sha := sha256.Sum256(body)
			row.Sha256 = hex.EncodeToString(sha[:])
			row.Width, row.Height, row.Bytes = w, h, uint32(len(body))
			ext := "jpg"
			switch format {
			case "png":
				ext = "png"
			case "gif":
				ext = "gif"
			}
			path := filepath.Join(f.Root, "raw", row.Sha256[:2], row.Sha256+"."+ext)
			if err := writeFileAtomic(path, body); err != nil {
				return row, err // disk trouble is the caller's to see
			}
			row.Status = StatusOK
			return row, nil
		}
		if ctx.Err() != nil {
			return row, ctx.Err()
		}
		var bad errBadImage
		if errors.As(err, &bad) {
			// The server sent bytes, but they are not a picture. No point retrying.
			row.Status = StatusBadImage
			row.Error = errString(err)
			return row, nil
		}
		var se *StatusError
		if errors.As(err, &se) {
			if goneCodes[se.Code] {
				row.Status = StatusImgGone
				row.Error = se.Error()
				return row, nil
			}
			// A rate limit the server timed itself: wait what it asked for.
			if se.Code == http.StatusTooManyRequests && se.RetryAfter >= 0 {
				wait = time.Duration(se.RetryAfter) * time.Second
			}
		}
		if attempt >= MaxAttempts-1 {
			row.Status = StatusErrorRow
			row.Error = errString(err)
			return row, nil
		}
		if wait == 0 {
			wait = time.Second // double from 1s next time, not from 0
		}
		wait = min(wait*2, 30*time.Second)
		select {
		case <-ctx.Done():
			return row, ctx.Err()
		case <-time.After(wait):
		}
	}
}

// download fetches url and reads it whole (at most maxBytes). It returns the bytes,
// their pixel size, and the decoded format ("jpeg", "png", or "gif"). A 200 whose
// bytes are not a decodable image is a bad image, reported as errBadImage. Anything
// the server said about itself is returned as a *StatusError; network trouble is
// returned as is, for the caller to retry.
func (f *Fetcher) download(ctx context.Context, client *http.Client, url string) (body []byte, w, h uint16, format string, err error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, url, nil)
	if err != nil {
		return nil, 0, 0, "", err
	}
	req.Header.Set("User-Agent", UserAgent)
	resp, err := client.Do(req)
	if err != nil {
		return nil, 0, 0, "", err
	}
	defer resp.Body.Close()
	body, rerr := io.ReadAll(io.LimitReader(resp.Body, maxBytes))
	if resp.StatusCode != http.StatusOK {
		retryAfter := -1
		if s, err := strconv.Atoi(resp.Header.Get("Retry-After")); err == nil && s >= 0 {
			retryAfter = s
		}
		return nil, 0, 0, "", &StatusError{Code: resp.StatusCode, Body: resp.Status, RetryAfter: retryAfter}
	}
	if rerr != nil {
		return nil, 0, 0, "", rerr
	}
	cfg, format, derr := image.DecodeConfig(bytes.NewReader(body))
	if derr != nil || cfg.Width < 1 || cfg.Height < 1 {
		return nil, 0, 0, "", errBadImage{err: derr.Error()}
	}
	return body, uint16(min(cfg.Width, 65535)), uint16(min(cfg.Height, 65535)), format, nil
}

// errBadImage marks bytes that are not a decodable image.
type errBadImage struct{ err string }

func (e errBadImage) Error() string { return "bad image: " + e.err }

// writeFileAtomic writes b to path via a temp file in the same directory and a rename.
// If the file already exists it is left alone (the bytes are the same by construction).
func writeFileAtomic(path string, b []byte) error {
	if _, err := os.Stat(path); err == nil {
		return nil
	}
	dir := filepath.Dir(path)
	if err := os.MkdirAll(dir, 0o755); err != nil {
		return err
	}
	tmp, err := os.CreateTemp(dir, ".tmp-*")
	if err != nil {
		return err
	}
	defer os.Remove(tmp.Name())
	if _, err := tmp.Write(b); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tmp.Name(), path); err != nil {
		return err
	}
	return nil
}

func errString(err error) string {
	s := err.Error()
	if len(s) > 200 {
		s = s[:200]
	}
	return s
}
