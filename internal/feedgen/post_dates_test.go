package feedgen

import (
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

func postAt(t time.Time) string {
	return "at://did:plc:abc/app.bsky.feed.post/" + syntax.NewTIDFromTime(t, 0).String()
}

func TestIndexedSince(t *testing.T) {
	now := time.Date(2026, 10, 8, 12, 0, 0, 0, time.UTC)
	cases := []struct {
		name   string
		uris   []string
		want   time.Time
		wantOK bool
	}{
		{"none", nil, time.Time{}, false},
		{"oldest post less a day", []string{postAt(now.Add(-2 * time.Hour)), postAt(now.Add(-30 * time.Hour))}, now.Add(-54 * time.Hour), true},
		{"backdated post widens the range", []string{postAt(now), postAt(time.Date(2023, 1, 1, 0, 0, 0, 0, time.UTC))},
			time.Date(2022, 12, 31, 0, 0, 0, 0, time.UTC), true},
		{"a few minutes ahead is fine", []string{postAt(now.Add(10 * time.Minute))}, now.Add(10*time.Minute - 24*time.Hour), true},
		{"TID from the future", []string{postAt(now), postAt(time.Date(2364, 3, 2, 0, 0, 0, 0, time.UTC))}, time.Time{}, false},
		{"record key not a TID", []string{postAt(now), "at://did:plc:abc/app.bsky.feed.post/self"}, time.Time{}, false},
		{"not a URI", []string{"nonsense"}, time.Time{}, false},
	}
	for _, c := range cases {
		got, ok := indexedSince(c.uris, now)
		if ok != c.wantOK || !got.Equal(c.want) {
			t.Errorf("%s: got %v, %v; want %v, %v", c.name, got, ok, c.want, c.wantOK)
		}
	}
}
