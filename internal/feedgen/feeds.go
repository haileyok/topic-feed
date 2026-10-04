package feedgen

import (
	"context"
	"errors"
	"log/slog"
	"reflect"
	"sync"
	"sync/atomic"
	"time"
)

// Builder fetches one feed's candidate posts. *Store implements it.
type Builder interface {
	Build(ctx context.Context, f Feed, since time.Time, limit int) ([]Post, Removed, error)
}

// Feeds keeps every feed ranked in memory. Requests read the latest build and never query
// ClickHouse, except for a feed nobody has asked for lately: see below. Recent builds are kept so a
// reader paging through one keeps getting pages of the same order.
//
// The feeds can change while the service runs (Reconcile), and are of two kinds. The service owner's
// are rebuilt on a timer for as long as they exist. Anyone else's (Feed.Owner is set) are built
// when somebody first asks for them, rebuilt only while somebody keeps asking, and let go when
// nobody has for a while: what a feed costs is a query over a day of posts, and there is no limit
// to how many feeds people can make.
type Feeds struct {
	Builder  Builder
	Log      *slog.Logger
	Window   time.Duration // posts at most this old
	MaxPosts int           // per feed
	Every    time.Duration // rebuild interval of the owner's feeds
	Keep     time.Duration // how long a build stays pageable

	// UserEvery is how often a feed of someone else's is rebuilt while it is in use, Idle how long it
	// is kept without anybody asking for it, and BuildWait how long a request waits for the first
	// build of a feed. MaxBuilds bounds how many of those feeds are being built at once.
	UserEvery time.Duration
	Idle      time.Duration
	BuildWait time.Duration
	MaxBuilds int

	mu      sync.RWMutex
	feeds   []Feed // every feed, in order
	state   map[string]*feedState
	ctx     context.Context // set by Start
	slots   chan struct{}
	syncNow chan struct{}
}

type feedState struct {
	feed     Feed         // what this state was made for; a changed feed gets a new state
	lastUsed atomic.Int64 // unix nanoseconds of the last request

	mu       sync.RWMutex
	current  *build
	older    []*build      // oldest first, only URIs kept
	building chan struct{} // closed when the build under way ends; nil when none is
	cancel   context.CancelFunc
	stopped  bool
}

type build struct {
	id      int64 // unix milliseconds of the build; unique per feed
	builtAt time.Time
	posts   []Post // full details: current build only
	items   []Item
}

// NewFeeds prepares the feeds from the config. Call Start before serving.
func NewFeeds(cfg *Config, b Builder, log *slog.Logger, window, every time.Duration, maxPosts int) *Feeds {
	fs := &Feeds{Builder: b, Log: log, Window: window, MaxPosts: maxPosts, Every: every, Keep: 15 * time.Minute,
		UserEvery: time.Minute, Idle: 10 * time.Minute, BuildWait: 4 * time.Second, MaxBuilds: 4,
		state: map[string]*feedState{}, syncNow: make(chan struct{}, 1)}
	fs.slots = make(chan struct{}, fs.MaxBuilds)
	fs.set(cfg.Feeds)
	return fs
}

// set replaces the feeds, keeping the state of those that are unchanged. It returns the states made
// new that are the owner's, which need building. The caller holds fs.mu.
func (fs *Feeds) set(feeds []Feed) (fresh []*feedState) {
	next := make(map[string]*feedState, len(feeds))
	ordered := make([]Feed, 0, len(feeds))
	for _, f := range feeds {
		key := f.Key()
		if _, dup := next[key]; dup {
			continue
		}
		ordered = append(ordered, f)
		if old := fs.state[key]; old != nil && reflect.DeepEqual(old.feed, f) {
			next[key] = old
			continue
		}
		st := &feedState{feed: f}
		next[key] = st
		if !f.lazy() && f.Personal == nil {
			fresh = append(fresh, st)
		}
	}
	for key, old := range fs.state {
		if next[key] != old {
			old.stop() // gone, or replaced by a changed feed
		}
	}
	fs.feeds, fs.state = ordered, next
	return fresh
}

// Reconcile makes these the feeds served: new ones appear, ones that are no longer listed go, and a
// changed one starts afresh. Feeds that are unchanged keep their builds.
func (fs *Feeds) Reconcile(feeds []Feed) {
	fs.mu.Lock()
	fresh := fs.set(feeds)
	ctx := fs.ctx
	fs.mu.Unlock()
	if ctx == nil {
		return // not started: Start builds them
	}
	for _, st := range fresh {
		go fs.warm(ctx, st)
	}
}

// List returns the service owner's feeds in order: the ones that are kept built. (Other people's
// can be looked up, but there is no list of them here.)
func (fs *Feeds) List() []Feed {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	out := make([]Feed, 0, len(fs.feeds))
	for _, f := range fs.feeds {
		if !f.lazy() {
			out = append(out, f)
		}
	}
	return out
}

// maxDescribedUserFeeds is how many feeds of other people describeFeedGenerator lists.
const maxDescribedUserFeeds = 200

// UserFeeds returns up to limit of the feeds of other people, the oldest first.
func (fs *Feeds) UserFeeds(limit int) []Feed {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	var out []Feed
	for _, f := range fs.feeds {
		if f.lazy() {
			if len(out) >= limit {
				break
			}
			out = append(out, f)
		}
	}
	return out
}

// Lookup returns the feed with this key (Feed.Key).
func (fs *Feeds) Lookup(key string) (Feed, bool) {
	if st := fs.stateOf(key); st != nil {
		return st.feed, true
	}
	return Feed{}, false
}

func (fs *Feeds) stateOf(key string) *feedState {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	return fs.state[key]
}

// Posts returns a feed's current ranked posts and when they were built. ok is false for
// an unknown feed; posts is nil before the first successful build.
func (fs *Feeds) Posts(key string) (posts []Post, builtAt time.Time, ok bool) {
	st := fs.stateOf(key)
	if st == nil {
		return nil, time.Time{}, false
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	if st.current == nil {
		return nil, time.Time{}, true
	}
	return st.current.posts, st.current.builtAt, true
}

var errUnknownFeed = errors.New("unknown feed")

// Page returns up to limit post URIs after the cursor ("" for the first page) and the
// cursor for the next page ("" at the end). A cursor whose build has expired continues
// at the same position in the current build. ready is false before the first build, which for
// a feed of someone else's is built now, if it can be in time.
func (fs *Feeds) Page(ctx context.Context, key, cursor string, limit int) (items []Item, next string, ready bool, err error) {
	st := fs.stateOf(key)
	if st == nil {
		return nil, "", true, errUnknownFeed
	}
	if st.feed.lazy() {
		st.lastUsed.Store(time.Now().UnixNano())
		if !fs.ensure(ctx, st) {
			return nil, "", false, nil
		}
	}
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

// stop ends the state's rebuilding on a timer, if it has one.
func (st *feedState) stop() {
	st.mu.Lock()
	defer st.mu.Unlock()
	st.stopped = true
	if st.cancel != nil {
		st.cancel()
		st.cancel = nil
	}
}

// Start builds the owner's feeds once, then keeps rebuilding them in the background until ctx
// ends, and tends the feeds of other people. A failed first build is logged and retried on the timer.
func (fs *Feeds) Start(ctx context.Context) {
	fs.mu.Lock()
	fs.ctx = ctx
	var pinned []*feedState
	for _, f := range fs.feeds {
		if !f.lazy() && f.Personal == nil { // a personal feed is built per viewer by Personal
			pinned = append(pinned, fs.state[f.Key()])
		}
	}
	fs.mu.Unlock()
	var wg sync.WaitGroup
	for _, st := range pinned {
		wg.Add(1)
		go func() {
			defer wg.Done()
			fs.refresh(ctx, st)
		}()
	}
	wg.Wait()
	for _, st := range pinned {
		fs.tick(ctx, st)
	}
	go fs.tend(ctx)
}

// warm builds a feed that has just appeared, then keeps it rebuilt.
func (fs *Feeds) warm(ctx context.Context, st *feedState) {
	fs.refresh(ctx, st)
	fs.tick(ctx, st)
}

// tick rebuilds the state's feed every fs.Every until the state is stopped or ctx ends.
func (fs *Feeds) tick(ctx context.Context, st *feedState) {
	st.mu.Lock()
	if st.stopped || st.cancel != nil {
		st.mu.Unlock()
		return
	}
	cctx, cancel := context.WithCancel(ctx)
	st.cancel = cancel
	st.mu.Unlock()
	go func() {
		t := time.NewTicker(fs.Every)
		defer t.Stop()
		for {
			select {
			case <-cctx.Done():
				return
			case <-t.C:
				fs.refresh(cctx, st)
			}
		}
	}()
}

// runCtx is the context builds that outlive a request run in.
func (fs *Feeds) runCtx() context.Context {
	fs.mu.RLock()
	defer fs.mu.RUnlock()
	if fs.ctx != nil {
		return fs.ctx
	}
	return context.Background()
}

// kick starts building a feed of someone else's unless a build is under way, and returns a channel
// that is closed when that build ends. At most MaxBuilds run at once; the rest wait their turn.
func (fs *Feeds) kick(st *feedState) <-chan struct{} {
	st.mu.Lock()
	defer st.mu.Unlock()
	if st.building != nil {
		return st.building
	}
	done := make(chan struct{})
	st.building = done
	go func() {
		defer func() {
			st.mu.Lock()
			st.building = nil
			st.mu.Unlock()
			close(done)
		}()
		ctx := fs.runCtx()
		select {
		case fs.slots <- struct{}{}:
		case <-ctx.Done():
			return
		}
		defer func() { <-fs.slots }()
		fs.refresh(ctx, st)
	}()
	return done
}

// ensure makes sure a feed of someone else's has a build, building it if it has none, and waits for
// that for at most BuildWait (or until ctx ends). It reports whether the feed has a build.
func (fs *Feeds) ensure(ctx context.Context, st *feedState) bool {
	st.mu.RLock()
	have := st.current != nil
	st.mu.RUnlock()
	if !have {
		timer := time.NewTimer(fs.BuildWait)
		defer timer.Stop()
		select {
		case <-fs.kick(st):
		case <-ctx.Done():
		case <-timer.C:
		}
	}
	st.mu.RLock()
	defer st.mu.RUnlock()
	return st.current != nil
}

// tend rebuilds the feeds of other people that are in use, and lets go of those that aren't, until
// ctx ends.
func (fs *Feeds) tend(ctx context.Context) {
	t := time.NewTicker(max(fs.UserEvery/4, time.Second))
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			fs.tendOnce(now)
		}
	}
}

func (fs *Feeds) tendOnce(now time.Time) {
	fs.mu.RLock()
	var lazy []*feedState
	for _, st := range fs.state {
		if st.feed.lazy() {
			lazy = append(lazy, st)
		}
	}
	fs.mu.RUnlock()
	for _, st := range lazy {
		idle := now.Sub(time.Unix(0, st.lastUsed.Load())) > fs.Idle
		stale := false
		st.mu.Lock()
		switch {
		case st.current == nil:
		case idle:
			st.current, st.older = nil, nil // the next request builds it again
		case now.Sub(st.current.builtAt) >= fs.UserEvery:
			stale = true
		}
		st.mu.Unlock()
		if stale {
			fs.kick(st)
		}
	}
}

func (fs *Feeds) refresh(ctx context.Context, st *feedState) {
	f := st.feed
	label := f.metricLabel()
	start := time.Now()
	bctx, cancel := context.WithTimeout(ctx, max(fs.Every, 10*time.Second))
	defer cancel()
	limit := fs.MaxPosts
	if f.MaxPosts > 0 {
		limit = f.MaxPosts
	}
	posts, rm, err := fs.Builder.Build(bctx, f, start.Add(-fs.Window), limit)
	metricRefreshSeconds.WithLabelValues(label).Observe(time.Since(start).Seconds())
	if err != nil {
		if ctx.Err() == nil {
			metricRefreshErrors.WithLabelValues(label).Inc()
			fs.Log.Error("feed refresh failed", "feed", f.Key(), "err", err)
		}
		return
	}
	ranked := Rank(posts, f, start)
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

	if f.lazy() {
		fs.Log.Debug("feed refreshed", "feed", f.Key(), "posts", len(posts), "took", time.Since(start).Round(time.Millisecond))
		return
	}
	metricFeedPosts.WithLabelValues(label).Set(float64(len(posts)))
	metricRemoved.WithLabelValues(label, "deleted").Set(float64(rm.Deleted))
	metricRemoved.WithLabelValues(label, "inactive").Set(float64(rm.Inactive))
	metricRemoved.WithLabelValues(label, "labeled").Set(float64(rm.Labeled))
	metricLastRefresh.WithLabelValues(label).Set(float64(start.Unix()))
	fs.Log.Debug("feed refreshed", "feed", f.Key(), "posts", len(posts), "removed", rm,
		"took", time.Since(start).Round(time.Millisecond))
}

// SyncFrom keeps the feeds as load says they are, asking now and then (every) and whenever SyncNow is
// called, until ctx ends. A failed load is logged and the feeds are left as they are.
func (fs *Feeds) SyncFrom(ctx context.Context, load func(context.Context) ([]Feed, error), every time.Duration) {
	t := time.NewTicker(every)
	defer t.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-t.C:
		case <-fs.syncNow:
		}
		lctx, cancel := context.WithTimeout(ctx, 30*time.Second)
		feeds, err := load(lctx)
		cancel()
		if err != nil {
			if ctx.Err() == nil {
				fs.Log.Error("loading the feeds failed; keeping the ones served", "err", err)
			}
			continue
		}
		// Every feed gone at once is far more likely a database that answered wrongly than somebody
		// removing them all, and serving nothing is worse than serving what was.
		topics := func(list []Feed) (n int) { // the personal feed is not from the database, so it is always there
			for _, f := range list {
				if f.Personal == nil {
					n++
				}
			}
			return n
		}
		fs.mu.RLock()
		serving := topics(fs.feeds)
		fs.mu.RUnlock()
		if topics(feeds) == 0 && serving > 0 {
			fs.Log.Error("the feeds loaded are none; keeping the ones served")
			continue
		}
		fs.Reconcile(feeds)
	}
}

// SyncNow asks SyncFrom to look at the feeds again straight away (after a feed was saved).
func (fs *Feeds) SyncNow() {
	select {
	case fs.syncNow <- struct{}{}:
	default: // a look is already waiting
	}
}
