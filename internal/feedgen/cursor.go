package feedgen

import (
	"errors"
	"sort"
	"strconv"
	"strings"
	"time"
)

// Post is one feed entry. Feeds are newest first, ordered by (IndexedAt, URI) descending,
// so the pair is a stable position even as new posts arrive at the top.
type Post struct {
	URI       string
	IndexedAt time.Time
	Score     float32 // highest probability among the feed's paths
}

// before reports whether a sorts after b in a feed (older, or same time and smaller URI).
func before(aT int64, aURI string, bT int64, bURI string) bool {
	if aT != bT {
		return aT < bT
	}
	return aURI < bURI
}

// Cursors are "<indexed_at in Unix microseconds>::<post URI>": the last post of the
// previous page. Clients treat them as opaque.

func encodeCursor(p Post) string {
	return strconv.FormatInt(p.IndexedAt.UnixMicro(), 10) + "::" + p.URI
}

var errBadCursor = errors.New("malformed cursor")

func decodeCursor(s string) (int64, string, error) {
	ts, uri, ok := strings.Cut(s, "::")
	if !ok || !strings.HasPrefix(uri, "at://") {
		return 0, "", errBadCursor
	}
	t, err := strconv.ParseInt(ts, 10, 64)
	if err != nil || t <= 0 {
		return 0, "", errBadCursor
	}
	return t, uri, nil
}

// page returns up to limit posts after the cursor ("" for the first page), and the cursor
// for the next page ("" when there are no more posts).
func page(posts []Post, cursor string, limit int) ([]Post, string, error) {
	start := 0
	if cursor != "" {
		t, uri, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", err
		}
		// posts is sorted newest first: find the first post that sorts after the cursor.
		start = sort.Search(len(posts), func(i int) bool {
			return before(posts[i].IndexedAt.UnixMicro(), posts[i].URI, t, uri)
		})
	}
	end := min(start+limit, len(posts))
	out := posts[start:end]
	next := ""
	if end < len(posts) && len(out) > 0 {
		next = encodeCursor(out[len(out)-1])
	}
	return out, next, nil
}
