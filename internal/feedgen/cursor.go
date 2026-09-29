package feedgen

import (
	"encoding/json"
	"errors"
	"math"
	"strconv"
	"strings"
	"time"
)

// Post is one feed candidate with what ranking needs.
type Post struct {
	URI       string
	DID       string
	IndexedAt time.Time
	Match     float32 // highest probability among the feed's paths
	TopPath   string  // the model's top subtopic path for the post
	TopPathP  float32 // its probability

	// Model signals, 0-1.
	Substance, GeneralInterest, Promo float32
	// Engagement so far.
	Likes, Reposts, Replies, Quotes uint64

	Score float64 // ranking score when the feed was built
}

// Item is one entry of a feed page.
type Item struct {
	URI string
	// Context is the post's feedContext: JSON {"id": post URI, "topic": the model's top
	// subtopic, "p": its probability}. Bluesky passes it through to the app and back to
	// the feed generator with interactions.
	Context string
}

func feedContext(p Post) string {
	b, _ := json.Marshal(struct {
		ID    string  `json:"id"`
		Topic string  `json:"topic"`
		P     float64 `json:"p"`
	}{p.URI, p.TopPath, math.Round(float64(p.TopPathP)*1000) / 1000})
	return string(b)
}

// A ranked feed changes on every rebuild, so a cursor names the build a reader started
// on and a position in it: "<build id>:<offset>". Builds are kept for a while (see
// Feeds.Keep), so paging through one is stable: no repeats, no gaps.

func encodeCursor(build int64, offset int) string {
	return strconv.FormatInt(build, 10) + ":" + strconv.Itoa(offset)
}

var errBadCursor = errors.New("malformed cursor")

func decodeCursor(s string) (build int64, offset int, err error) {
	b, o, ok := strings.Cut(s, ":")
	if !ok {
		return 0, 0, errBadCursor
	}
	build, err1 := strconv.ParseInt(b, 10, 64)
	offset, err2 := strconv.Atoi(o)
	if err1 != nil || err2 != nil || build <= 0 || offset < 0 {
		return 0, 0, errBadCursor
	}
	return build, offset, nil
}
