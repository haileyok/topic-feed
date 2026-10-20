package feedgen

import (
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// indexedSince is the earliest ingest time (indexed_at) the posts in uris can have, so that a lookup of
// them by URI can skip the days of post_pipeline they can't be in. A post's record key is a TID, the
// time its author's server made it, and a post is ingested after it is made; a day is allowed for
// servers whose clocks are off. A backdated TID (an imported post) only makes the range wider.
//
// ok is false, and the lookup should read every day, when that can't be relied on: no URIs, a record
// key that isn't a TID, or a TID in the future (a post indexed before its TID's time).
func indexedSince(uris []string, now time.Time) (since time.Time, ok bool) {
	if len(uris) == 0 {
		return time.Time{}, false
	}
	for _, uri := range uris {
		i := strings.LastIndexByte(uri, '/')
		if i < 0 {
			return time.Time{}, false
		}
		tid, err := syntax.ParseTID(uri[i+1:])
		if err != nil {
			return time.Time{}, false
		}
		t := tid.Time()
		if t.After(now.Add(time.Hour)) {
			return time.Time{}, false
		}
		if since.IsZero() || t.Before(since) {
			since = t
		}
	}
	return since.Add(-24 * time.Hour), true
}
