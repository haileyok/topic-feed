package feedgen

import (
	"errors"
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

	// Model signals, 0-1.
	Substance, GeneralInterest, Promo float32
	// Engagement so far.
	Likes, Reposts, Replies, Quotes uint64

	Score float64 // ranking score when the feed was built
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
