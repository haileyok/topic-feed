package feedgen

import (
	"cmp"
	"encoding/json"
	"errors"
	"math"
	"slices"
	"strconv"
	"strings"
	"time"
)

// Post is one feed candidate with what ranking needs.
type Post struct {
	URI       string
	DID       string
	IndexedAt time.Time
	Match     float32   // highest probability among the feed's paths
	TopPath   string    // the model's top subtopic path for the post
	TopPathP  float32   // its probability
	TopPaths  []string  // the model's three most likely subtopic paths, best first
	TopPs     []float32 // their probabilities
	Tone      map[string]float32

	Signals map[string]float32 // the model's signal scores, 0-1
	Labels  []string           // self-labels and moderation labels on the post and author when processed
	// Engagement so far.
	Likes, Reposts, Replies, Quotes uint64

	Score float64 // ranking score when the feed was built
}

// Engagement is the post's likes, reposts, replies, and quotes, each counted at its weight. It
// is what ranking adds to a post's score, and what a personal feed's minimum is measured in.
func (p Post) Engagement(w Weights) float64 {
	return w.Like*float64(p.Likes) + w.Repost*float64(p.Reposts) + w.Reply*float64(p.Replies) + w.Quote*float64(p.Quotes)
}

// Item is one entry of a feed page.
type Item struct {
	URI string
	// Context is the post's feedContext: JSON {"id": post URI, "topic": the model's top
	// subtopic, "p": its probability, "top": [[subtopic, probability], ...] for the three
	// most likely, "tone": [[tone, probability], ...] for all six, most likely first}.
	// Bluesky passes it through to the app and back to the feed generator with
	// interactions.
	Context string
}

func feedContext(p Post) string {
	top := make([][2]any, 0, len(p.TopPaths))
	for i, path := range p.TopPaths {
		if i < len(p.TopPs) {
			top = append(top, [2]any{path, round3(p.TopPs[i])})
		}
	}
	tone := make([][2]any, 0, len(p.Tone))
	for _, name := range Tones {
		if v, ok := p.Tone[name]; ok {
			tone = append(tone, [2]any{name, round3(v)})
		}
	}
	slices.SortStableFunc(tone, func(a, b [2]any) int { return cmp.Compare(b[1].(float64), a[1].(float64)) })
	b, _ := json.Marshal(struct {
		ID    string   `json:"id"`
		Topic string   `json:"topic"`
		P     float64  `json:"p"`
		Top   [][2]any `json:"top"`
		Tone  [][2]any `json:"tone"`
	}{p.URI, p.TopPath, round3(p.TopPathP), top, tone})
	return string(b)
}

func round3(x float32) float64 { return math.Round(float64(x)*1000) / 1000 }

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
