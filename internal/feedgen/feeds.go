package feedgen

import (
	"context"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"
)

// Builder builds one feed's posts. *Store implements it.
type Builder interface {
	Build(ctx context.Context, f Feed, since time.Time, limit int) ([]Post, Removed, error)
}

// Feeds keeps every feed's posts in memory, rebuilt on a timer. Requests read the latest
// snapshot and never query ClickHouse.
type Feeds struct {
	Builder  Builder
	Log      *slog.Logger
	Window   time.Duration // posts at most this old
	MaxPosts int           // per feed
	Every    time.Duration // rebuild interval

	feeds []Feed
	snaps map[string]*atomic.Pointer[snapshot]
}

type snapshot struct {
	posts   []Post
	builtAt time.Time
}

// NewFeeds prepares the feeds from the config. Call Start before serving.
func NewFeeds(cfg *Config, b Builder, log *slog.Logger, window, every time.Duration, maxPosts int) *Feeds {
	fs := &Feeds{Builder: b, Log: log, Window: window, MaxPosts: maxPosts, Every: every,
		feeds: cfg.Feeds, snaps: map[string]*atomic.Pointer[snapshot]{}}
	for _, f := range cfg.Feeds {
		fs.snaps[f.Rkey] = &atomic.Pointer[snapshot]{}
	}
	return fs
}

// List returns the configured feeds in config order.
func (fs *Feeds) List() []Feed { return fs.feeds }

// Posts returns a feed's current posts and when they were built. ok is false for an
// unknown feed; posts is nil before the first successful build.
func (fs *Feeds) Posts(rkey string) (posts []Post, builtAt time.Time, ok bool) {
	p, ok := fs.snaps[rkey]
	if !ok {
		return nil, time.Time{}, false
	}
	if s := p.Load(); s != nil {
		return s.posts, s.builtAt, true
	}
	return nil, time.Time{}, true
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
	fs.snaps[f.Rkey].Store(&snapshot{posts: posts, builtAt: start})
	metricFeedPosts.WithLabelValues(f.Rkey).Set(float64(len(posts)))
	metricRemoved.WithLabelValues(f.Rkey, "deleted").Set(float64(rm.Deleted))
	metricRemoved.WithLabelValues(f.Rkey, "inactive").Set(float64(rm.Inactive))
	metricRemoved.WithLabelValues(f.Rkey, "labeled").Set(float64(rm.Labeled))
	metricLastRefresh.WithLabelValues(f.Rkey).Set(float64(start.Unix()))
	fs.Log.Debug("feed refreshed", "feed", f.Rkey, "posts", len(posts), "removed", rm,
		"took", time.Since(start).Round(time.Millisecond))
}
