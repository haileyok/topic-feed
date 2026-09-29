package feedgen

import (
	"context"
	"log/slog"
	"sync"
	"time"
)

// Builder fetches one feed's candidate posts. *Store implements it.
type Builder interface {
	Build(ctx context.Context, f Feed, since time.Time, limit int) ([]Post, Removed, error)
}

// Feeds keeps every feed ranked in memory, rebuilt on a timer. Requests read the latest
// build and never query ClickHouse. Recent builds are kept so a reader paging through one
// keeps getting pages of the same order.
type Feeds struct {
	Builder  Builder
	Log      *slog.Logger
	Window   time.Duration // posts at most this old
	MaxPosts int           // per feed
	Every    time.Duration // rebuild interval
	Keep     time.Duration // how long a build stays pageable

	feeds []Feed
	state map[string]*feedState
}

type feedState struct {
	mu      sync.RWMutex
	current *build
	older   []*build // oldest first, only URIs kept
}

type build struct {
	id      int64 // unix milliseconds of the build; unique per feed
	builtAt time.Time
	posts   []Post // full details: current build only
	items   []Item
}

// NewFeeds prepares the feeds from the config. Call Start before serving.
func NewFeeds(cfg *Config, b Builder, log *slog.Logger, window, every time.Duration, maxPosts int) *Feeds {
	fs := &Feeds{Builder: b, Log: log, Window: window, MaxPosts: maxPosts, Every: every,
		Keep: 15 * time.Minute, feeds: cfg.Feeds, state: map[string]*feedState{}}
	for _, f := range cfg.Feeds {
		fs.state[f.Rkey] = &feedState{}
	}
	return fs
}

// List returns the configured feeds in config order.
func (fs *Feeds) List() []Feed { return fs.feeds }

// Posts returns a feed's current ranked posts and when they were built. ok is false for
// an unknown feed; posts is nil before the first successful build.
func (fs *Feeds) Posts(rkey string) (posts []Post, builtAt time.Time, ok bool) {
	st, ok := fs.state[rkey]
	if !ok {
		return nil, time.Time{}, false
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	if st.current == nil {
		return nil, time.Time{}, true
	}
	return st.current.posts, st.current.builtAt, true
}

// Page returns up to limit post URIs after the cursor ("" for the first page) and the
// cursor for the next page ("" at the end). A cursor whose build has expired continues
// at the same position in the current build. ready is false before the first build.
func (fs *Feeds) Page(rkey, cursor string, limit int) (items []Item, next string, ready bool, err error) {
	st := fs.state[rkey]
	b, offset := (*build)(nil), 0
	if cursor != "" {
		id, off, err := decodeCursor(cursor)
		if err != nil {
			return nil, "", true, err
		}
		offset = off
		st.mu.RLock()
		b = st.find(id)
		st.mu.RUnlock()
	}
	if b == nil {
		st.mu.RLock()
		b = st.current
		st.mu.RUnlock()
	}
	if b == nil {
		return nil, "", false, nil
	}
	if offset >= len(b.items) {
		return []Item{}, "", true, nil
	}
	end := min(offset+limit, len(b.items))
	if end < len(b.items) {
		next = encodeCursor(b.id, end)
	}
	return b.items[offset:end], next, true, nil
}

func (st *feedState) find(id int64) *build {
	if st.current != nil && st.current.id == id {
		return st.current
	}
	for _, b := range st.older {
		if b.id == id {
			return b
		}
	}
	return nil
}

// Start builds every feed once, then keeps rebuilding them in the background until ctx
// ends. A failed first build is logged and retried on the timer.
func (fs *Feeds) Start(ctx context.Context) {
	var wg sync.WaitGroup
	for _, f := range fs.feeds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fs.refresh(ctx, f)
		}()
	}
	wg.Wait()
	for _, f := range fs.feeds {
		go func() {
			t := time.NewTicker(fs.Every)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					fs.refresh(ctx, f)
				}
			}
		}()
	}
}

func (fs *Feeds) refresh(ctx context.Context, f Feed) {
	start := time.Now()
	bctx, cancel := context.WithTimeout(ctx, max(fs.Every, 10*time.Second))
	defer cancel()
	posts, rm, err := fs.Builder.Build(bctx, f, start.Add(-fs.Window), fs.MaxPosts)
	metricRefreshSeconds.WithLabelValues(f.Rkey).Observe(time.Since(start).Seconds())
	if err != nil {
		if ctx.Err() == nil {
			metricRefreshErrors.WithLabelValues(f.Rkey).Inc()
			fs.Log.Error("feed refresh failed", "feed", f.Rkey, "err", err)
		}
		return
	}
	ranked := Rank(posts, f.Ranking, f.Tone, start)
	st := fs.state[f.Rkey]
	st.mu.Lock()
	prev := st.current
	b := &build{id: start.UnixMilli(), builtAt: start, posts: ranked, items: make([]Item, len(ranked))}
	if prev != nil && b.id <= prev.id {
		b.id = prev.id + 1
	}
	// Share strings with the previous build, so kept builds cost little memory.
	var seen map[string]Item
	if prev != nil {
		seen = make(map[string]Item, len(prev.items))
		for _, it := range prev.items {
			seen[it.URI] = it
		}
	}
	for i, p := range ranked {
		it := Item{URI: p.URI, Context: feedContext(p)}
		if old, ok := seen[p.URI]; ok {
			it.URI = old.URI
			if old.Context == it.Context {
				it.Context = old.Context
			}
		}
		b.items[i] = it
	}
	if prev != nil {
		prev.posts = nil
		st.older = append(st.older, prev)
	}
	for len(st.older) > 0 && start.Sub(st.older[0].builtAt) > fs.Keep {
		st.older = st.older[1:]
	}
	st.current = b
	st.mu.Unlock()

	metricFeedPosts.WithLabelValues(f.Rkey).Set(float64(len(posts)))
	metricRemoved.WithLabelValues(f.Rkey, "deleted").Set(float64(rm.Deleted))
	metricRemoved.WithLabelValues(f.Rkey, "inactive").Set(float64(rm.Inactive))
	metricRemoved.WithLabelValues(f.Rkey, "labeled").Set(float64(rm.Labeled))
	metricRemoved.WithLabelValues(f.Rkey, "tone").Set(float64(rm.Tone))
	metricLastRefresh.WithLabelValues(f.Rkey).Set(float64(start.Unix()))
	fs.Log.Debug("feed refreshed", "feed", f.Rkey, "posts", len(posts), "removed", rm,
		"took", time.Since(start).Round(time.Millisecond))
}
