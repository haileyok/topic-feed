package feedgen

import (
	"context"
	"sync"
	"time"
)

// ScoreCache keeps what the model made of the posts filtered feeds have looked up, so that a page of
// a source feed only asks ClickHouse about posts no recent page has shown. Source feeds show the same
// posts again and again (to the same viewer while scrolling, and the popular posts to everyone), and a
// post's scores only change when it is processed again (a new model, a picture retry).
//
// A post the model hasn't scored is remembered only briefly: it may simply not have been processed
// yet, which the pipeline does within seconds to a minute of the post being ingested.
//
// It wraps a FilteredStore and passes everything else through.
type ScoreCache struct {
	FilteredStore
	// TTL is how long a scored post is kept, UnscoredTTL how long a post without scores is.
	TTL, UnscoredTTL time.Duration
	// Max is how many posts are kept at most.
	Max int

	now func() time.Time
	mu  sync.Mutex
	m   map[string]cachedScore
}

type cachedScore struct {
	post   ScoredPost
	scored bool
	until  time.Time
}

// NewScoreCache wraps store with a cache of up to 50,000 posts, each kept 10 minutes (30 seconds when
// the model hasn't scored it).
func NewScoreCache(store FilteredStore) *ScoreCache {
	return &ScoreCache{FilteredStore: store, TTL: 10 * time.Minute, UnscoredTTL: 30 * time.Second, Max: 50_000,
		now: time.Now, m: map[string]cachedScore{}}
}

func (c *ScoreCache) ScorePosts(ctx context.Context, uris []string) (map[string]ScoredPost, error) {
	now := c.now()
	out := make(map[string]ScoredPost, len(uris))
	var missing []string
	c.mu.Lock()
	for _, uri := range uris {
		if e, ok := c.m[uri]; ok && now.Before(e.until) {
			if e.scored {
				out[uri] = e.post
			}
			continue
		}
		missing = append(missing, uri)
	}
	c.mu.Unlock()
	metricScoreCache.WithLabelValues("hit").Add(float64(len(uris) - len(missing)))
	metricScoreCache.WithLabelValues("miss").Add(float64(len(missing)))
	if len(missing) == 0 {
		return out, nil
	}

	got, err := c.FilteredStore.ScorePosts(ctx, missing)
	if err != nil {
		return nil, err
	}
	c.mu.Lock()
	defer c.mu.Unlock()
	c.makeRoom(now, len(missing))
	for _, uri := range missing {
		if p, ok := got[uri]; ok {
			out[uri] = p
			c.m[uri] = cachedScore{post: p, scored: true, until: now.Add(c.TTL)}
		} else {
			c.m[uri] = cachedScore{until: now.Add(c.UnscoredTTL)}
		}
	}
	metricScoreCacheEntries.Set(float64(len(c.m)))
	return out, nil
}

// makeRoom makes space for n more posts: first by dropping expired ones, then, if that isn't enough,
// by dropping posts until the cache is half full (map order: which ones is arbitrary). Called with
// c.mu held.
func (c *ScoreCache) makeRoom(now time.Time, n int) {
	if len(c.m)+n <= c.Max {
		return
	}
	for uri, e := range c.m {
		if !now.Before(e.until) {
			delete(c.m, uri)
		}
	}
	if len(c.m)+n <= c.Max {
		return
	}
	for uri := range c.m {
		if len(c.m)+n <= c.Max/2 {
			break
		}
		delete(c.m, uri)
	}
}
