package feedgen

import (
	"context"
	"errors"
	"log/slog"
	"sync"
	"time"
)

const (
	// defaultBudget is how long a viewer's first request waits for their feed to be built
	// before showing the welcome post instead.
	defaultBudget = 1500 * time.Millisecond
	// profileTTL is how long a viewer's interests are used before being read again, in the
	// background, from their latest likes.
	profileTTL = 30 * time.Minute
	// retryAfter is how long a viewer whose feed failed to build sees the welcome post
	// before another attempt.
	retryAfter = 15 * time.Second
	// viewerIdle is how long a viewer's state is kept after their last request.
	viewerIdle = 6 * time.Hour
	// maxViewerReads is how many new viewers are read from the database at once; a rush of
	// first-time viewers (a feed being shared) queues rather than flooding ClickHouse.
	maxViewerReads = 8
)

var errNotReady = errors.New("personal feed is still loading")

// Personal serves the feeds configured with `personal:`: a feed built for each viewer from
// the posts they liked. A viewer's interests and what they've already seen are loaded the
// first time they ask (a second or so); until then they see the welcome post. After that
// every request is answered from memory.
type Personal struct {
	Source PersonalSource
	// Served stores the posts sent to viewers, so a restart doesn't forget them; nil: not stored.
	Served ServedSink
	// Tunings holds how viewers have adjusted their feeds; nil: nobody has, and SetTuning
	// can't save any.
	Tunings TuningStore
	Log     *slog.Logger
	// Welcome is the at:// URI of a real post shown while a viewer's feed is being built,
	// to anonymous viewers, and after a failed build; "" shows an empty feed instead.
	Welcome string
	// Budget is how long a request waits for a first build before showing the welcome post.
	Budget time.Duration
	// Every is how often the shared posts are refreshed (default 60s).
	Every time.Duration
	// Now is the clock; nil: time.Now.
	Now func() time.Time

	feeds   map[string]*personalFeed
	reads   chan struct{} // a slot for each viewer being read from the database
	mu      sync.Mutex    // guards viewers and each state's lastUsed
	viewers map[viewerKey]*viewerState
}

type viewerKey struct{ rkey, did string }

// personalFeed is one personal feed and the posts every viewer's feed draws on.
type personalFeed struct {
	feed Feed
	cfg  PersonalConfig

	mu sync.RWMutex
	// pools holds each subtopic's posts, best first, ranked once for every freshness setting
	// (see freshnessKey): a viewer's tuning picks one. All are set together.
	pools map[string]map[string][]Post
	// raw is the pool before any ranking: a viewer whose tuning ranks posts its own way gets a
	// ranked pool made from it.
	raw     map[string][]Post
	posts   int
	builtAt time.Time
}

// snapshot returns the pools, or nil before the posts have been read.
func (pf *personalFeed) snapshot() map[string]map[string][]Post {
	pf.mu.RLock()
	defer pf.mu.RUnlock()
	return pf.pools
}

// poolFor is the pool the viewer's feed is assembled from: one of the feed's, ranked once for
// everyone, or, when their tuning ranks posts in a way of its own, one ranked for them alone.
// It is nil before the posts have been read.
func (pf *personalFeed) poolFor(t Tuning, now time.Time) map[string][]Post {
	pf.mu.RLock()
	pools, raw := pf.pools, pf.raw
	pf.mu.RUnlock()
	if pools == nil {
		return nil
	}
	if !t.customRanking() {
		return pools[freshnessKey(t)]
	}
	return RankPoolWith(raw, t.RankingFor(pf.feed.Ranking), t.Tone, t.Signals, t.TopicRules, now)
}

// freshnessKey is which of a feed's pools a tuning ranks by: "" is the feed's own ranking.
func freshnessKey(t Tuning) string {
	if t.Freshness == "" {
		return FreshnessBalanced
	}
	return t.Freshness
}

// viewerState is everything kept about one viewer of one feed.
type viewerState struct {
	did      string
	ready    chan struct{} // closed when the first build has finished, well or not
	lastUsed time.Time     // guarded by Personal.mu

	mu       sync.Mutex
	built    bool  // the first build finished well
	err      error // why the first build failed
	failedAt time.Time
	// What their likes say, before tuning: how much of each subtopic they liked, on how many
	// classified posts, read the way massKey says (how far back, how fast likes fade).
	mass      map[string]float64
	likeCount int
	massKey   likeKey
	// tuning is how they've adjusted the feed. tuningSet: it was saved while the first build
	// was reading it, so it wins over what that read found. tuningUnknown: reading it failed,
	// so the feed is untuned until the next refresh, which tries again.
	tuning        Tuning
	tuningSet     bool
	tuningUnknown bool
	state         string    // what kind of feed `list` is: StatePersonal or StateGeneric
	refreshAfter  time.Time // when to read the interests again
	refreshing    bool
	liked         map[string]struct{} // posts they liked or reposted: never shown
	confirmed     map[string]struct{} // posts Bluesky reported them as having met
	early         map[string]struct{} // reported while the first build was still running
	serves        map[string]int      // times each post has been sent to them
	list          []Post              // their feed as last assembled
	listID        int64
}

// NewPersonal prepares the personal feeds among feeds. Call Start before serving.
func NewPersonal(feeds []Feed, src PersonalSource, log *slog.Logger) *Personal {
	p := &Personal{Source: src, Log: log, Budget: defaultBudget, Every: time.Minute,
		feeds: map[string]*personalFeed{}, viewers: map[viewerKey]*viewerState{},
		reads: make(chan struct{}, maxViewerReads)}
	for _, f := range feeds {
		if f.Personal != nil {
			p.feeds[f.Rkey] = &personalFeed{feed: f, cfg: *f.Personal}
		}
	}
	return p
}

func (p *Personal) now() time.Time {
	if p.Now != nil {
		return p.Now()
	}
	return time.Now()
}

// Serves reports whether rkey is a personal feed.
func (p *Personal) Serves(rkey string) bool {
	_, ok := p.feeds[rkey]
	return ok
}

// Status reports how many posts a personal feed draws on and when they were read; ok is
// false for a feed that isn't personal or hasn't been read yet.
func (p *Personal) Status(rkey string) (posts int, builtAt time.Time, ok bool) {
	pf := p.feeds[rkey]
	if pf == nil {
		return 0, time.Time{}, false
	}
	pf.mu.RLock()
	defer pf.mu.RUnlock()
	return pf.posts, pf.builtAt, pf.pools != nil
}

// Start reads every feed's posts once, then keeps refreshing them in the background until
// ctx ends. A failed first read is logged and retried on the timer.
func (p *Personal) Start(ctx context.Context) {
	var wg sync.WaitGroup
	for _, pf := range p.feeds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			p.refresh(ctx, pf)
		}()
	}
	wg.Wait()
	every := p.Every
	if every <= 0 {
		every = time.Minute
	}
	for _, pf := range p.feeds {
		go func() {
			t := time.NewTicker(every)
			defer t.Stop()
			for {
				select {
				case <-ctx.Done():
					return
				case <-t.C:
					p.refresh(ctx, pf)
				}
			}
		}()
	}
	go func() {
		t := time.NewTicker(10 * time.Minute)
		defer t.Stop()
		for {
			select {
			case <-ctx.Done():
				return
			case <-t.C:
				p.forgetIdle()
			}
		}
	}()
}

func (p *Personal) refresh(ctx context.Context, pf *personalFeed) {
	start := p.now()
	rctx, cancel := context.WithTimeout(ctx, max(p.Every, 30*time.Second))
	defer cancel()
	raw, err := p.Source.TopicPool(rctx, pf.cfg.PoolQuery(start, pf.feed.Ranking))
	metricPersonalPoolSeconds.WithLabelValues(pf.feed.Rkey).Observe(time.Since(start).Seconds())
	if err != nil {
		if ctx.Err() == nil {
			metricPersonalPoolErrors.WithLabelValues(pf.feed.Rkey).Inc()
			p.Log.Error("personal feed refresh failed", "feed", pf.feed.Rkey, "err", err)
		}
		return
	}
	pools := make(map[string]map[string][]Post, 3)
	for _, f := range []string{FreshnessPopular, FreshnessBalanced, FreshnessFresh} {
		pools[f] = RankPool(raw, Tuning{Freshness: f}.RankingFor(pf.feed.Ranking), start)
	}
	n := 0
	for _, ps := range pools[FreshnessBalanced] {
		n += len(ps)
	}
	pf.mu.Lock()
	pf.pools, pf.raw, pf.posts, pf.builtAt = pools, raw, n, start
	pf.mu.Unlock()
	metricPersonalPoolPosts.WithLabelValues(pf.feed.Rkey).Set(float64(n))
	p.Log.Debug("personal feed refreshed", "feed", pf.feed.Rkey, "posts", n, "topics", len(pools[FreshnessBalanced]),
		"took", time.Since(start).Round(time.Millisecond))
}

// forgetIdle drops the state of viewers who haven't been back for a while.
func (p *Personal) forgetIdle() {
	p.mu.Lock()
	defer p.mu.Unlock()
	cut := p.now().Add(-viewerIdle)
	for k, vs := range p.viewers {
		if vs.lastUsed.Before(cut) {
			delete(p.viewers, k)
		}
	}
	metricPersonalViewers.Set(float64(len(p.viewers)))
}

// PersonalPage is one page of a viewer's feed.
type PersonalPage struct {
	Items  []Item
	Cursor string
	// State is what the viewer got, for metrics: personal (their interests), generic (a mix
	// of every topic: they haven't liked enough), or welcome (the welcome post, or an empty
	// feed when none is set).
	State string
}

func (p *Personal) welcomePage() PersonalPage {
	items := []Item{}
	if p.Welcome != "" {
		items = append(items, Item{URI: p.Welcome})
	}
	return PersonalPage{Items: items, State: "welcome"}
}

// Page returns up to limit posts of the viewer's feed after the cursor ("" for the first
// page, which assembles a fresh feed from the latest posts, leaving out what they have
// already seen) and the cursor for the next page. An empty viewer means a request without
// a valid credential, which gets the welcome post.
func (p *Personal) Page(ctx context.Context, rkey, viewer, cursor string, limit int) (PersonalPage, error) {
	pf := p.feeds[rkey]
	if pf == nil {
		return PersonalPage{}, errNotReady
	}
	var listID int64
	var offset int
	if cursor != "" {
		var err error
		if listID, offset, err = decodeCursor(cursor); err != nil {
			return PersonalPage{}, err
		}
	}
	pools := pf.snapshot()
	if pools == nil {
		return PersonalPage{}, errNotReady
	}
	if viewer == "" {
		return p.welcomePage(), nil
	}

	vs := p.viewer(pf, viewer)
	budget := p.Budget
	if budget <= 0 {
		budget = defaultBudget
	}
	wait := time.NewTimer(budget)
	defer wait.Stop()
	select {
	case <-vs.ready:
	case <-wait.C:
		return p.welcomePage(), nil
	case <-ctx.Done():
		return PersonalPage{}, ctx.Err()
	}

	vs.mu.Lock()
	defer vs.mu.Unlock()
	if vs.err != nil {
		return p.welcomePage(), nil
	}
	now := p.now()
	if !vs.refreshing && now.After(vs.refreshAfter) {
		vs.refreshing = true
		go p.refreshProfile(pf, vs)
	}

	tuning := vs.tuning
	cfg := tuning.Config(pf.cfg)
	weights := pf.feed.Ranking.Weights // the feed's, not the viewer's: see belowMinEngagement
	skip := func(post Post) bool {
		if post.DID == vs.did {
			return true
		}
		if _, ok := vs.liked[post.URI]; ok {
			return true
		}
		if !tuning.ShowSeen { // a viewer can ask for posts they have seen to stay in their feed
			if _, ok := vs.confirmed[post.URI]; ok {
				return true
			}
			if vs.serves[post.URI] >= cfg.MaxServes {
				return true
			}
		}
		return tuning.Excludes(post, cfg, now) || belowMinEngagement(post, cfg, weights)
	}

	if cursor == "" || vs.list == nil || listID != vs.listID {
		// A first page, or a cursor from a feed we have since replaced: assemble again, from
		// the start. What the viewer was sent before counts toward what is left out, so
		// starting again doesn't repeat it.
		offset = 0
		pool := pf.poolFor(tuning, now)
		var prof Profile
		prof, vs.state = ProfileFor(cfg, tuning, vs.mass, vs.likeCount, pool, now)
		vs.list = Assemble(prof, pool, skip, cfg.ListSize, *cfg.AuthorGap)
		vs.listID = max(now.UnixMilli(), vs.listID+1)
	}

	page := PersonalPage{Items: []Item{}, State: vs.state}
	var rows []ServedRow
	i := offset
	for ; i < len(vs.list) && len(page.Items) < limit; i++ {
		post := vs.list[i]
		if skip(post) {
			continue
		}
		page.Items = append(page.Items, Item{URI: post.URI, Context: feedContext(post)})
		vs.serves[post.URI]++
		rows = append(rows, ServedRow{ViewerDID: vs.did, URI: post.URI, Feed: rkey, ServedAt: now.UTC()})
	}
	for j := i; j < len(vs.list); j++ {
		if !skip(vs.list[j]) {
			page.Cursor = encodeCursor(vs.listID, i)
			break
		}
	}
	if p.Served != nil && len(rows) > 0 {
		p.Served.Add(rows)
	}
	return page, nil
}

// viewer returns the viewer's state, starting to build it if there is none (or the last
// attempt failed a while ago).
func (p *Personal) viewer(pf *personalFeed, did string) *viewerState {
	key := viewerKey{pf.feed.Rkey, did}
	now := p.now()
	p.mu.Lock()
	defer p.mu.Unlock()
	vs := p.viewers[key]
	if vs != nil {
		vs.mu.Lock()
		stale := vs.err != nil && now.Sub(vs.failedAt) > retryAfter
		vs.mu.Unlock()
		if stale {
			vs = nil
		}
	}
	if vs == nil {
		vs = &viewerState{did: did, ready: make(chan struct{}), early: map[string]struct{}{}}
		p.viewers[key] = vs
		metricPersonalViewers.Set(float64(len(p.viewers)))
		go p.build(pf, vs)
	}
	vs.lastUsed = now
	return vs
}

// build reads a viewer's interests, what they liked, and what they've been shown.
func (p *Personal) build(pf *personalFeed, vs *viewerState) {
	defer close(vs.ready)
	p.reads <- struct{}{} // wait for a turn; viewers waiting see the welcome post meanwhile
	defer func() { <-p.reads }()
	start := p.now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg := pf.cfg
	since := start.AddDate(0, 0, -cfg.LookbackDays)

	var likes LikeData
	var seen SeenData
	var tuning Tuning
	var lerr, serr, terr error
	var wg sync.WaitGroup
	wg.Add(3)
	go func() {
		defer wg.Done()
		likes, lerr = p.Source.LikeProfile(ctx, vs.did, since, start, halfLifeDuration(cfg.HalfLifeDays))
	}()
	go func() {
		defer wg.Done()
		seen, serr = p.Source.ViewerSeen(ctx, vs.did, since)
	}()
	go func() {
		defer wg.Done()
		tuning, terr = p.readTuning(ctx, vs.did)
	}()
	wg.Wait()
	key := likeKey{cfg.HalfLifeDays, cfg.LookbackDays}
	if lerr == nil && terr == nil {
		if k := tuning.likeKey(cfg); k != key {
			// They chose how far back likes count, or how fast they fade: read them again that way.
			key = k
			likes, lerr = p.Source.LikeProfile(ctx, vs.did, start.AddDate(0, 0, -k.lookbackDays), start, halfLifeDuration(k.halfLifeDays))
		}
	}
	metricPersonalBuildSeconds.Observe(time.Since(start).Seconds())

	vs.mu.Lock()
	defer vs.mu.Unlock()
	if err := errors.Join(lerr, serr); err != nil {
		vs.err, vs.failedAt = err, start
		metricPersonalBuildErrors.Inc()
		p.Log.Error("personal feed: reading a viewer failed", "feed", pf.feed.Rkey, "viewer", vs.did, "err", err)
		return
	}
	// A failed read of their tuning doesn't fail the build: they get the feed untuned, and the
	// next refresh tries again.
	switch {
	case vs.tuningSet: // saved while this was reading: newer than what it found
	case terr != nil:
		vs.tuningUnknown = true
		metricPersonalTuningErrors.Inc()
		p.Log.Error("personal feed: reading a viewer's tuning failed", "feed", pf.feed.Rkey, "viewer", vs.did, "err", terr)
	default:
		vs.tuning = tuning
	}
	vs.mass, vs.likeCount, vs.massKey = likes.Mass, likes.Posts, key
	vs.refreshAfter = start.Add(profileTTL)
	if vs.tuningUnknown {
		vs.refreshAfter = start.Add(retryAfter)
	} else if vs.tuning.likeKey(cfg) != key {
		vs.refreshAfter = time.Time{} // their tuning changed while this read: read again
	}
	vs.liked = likes.Liked
	vs.confirmed = seen.Confirmed
	vs.serves = seen.Serves
	// Maps are written to below, and a source may return none.
	if vs.confirmed == nil {
		vs.confirmed = map[string]struct{}{}
	}
	if vs.serves == nil {
		vs.serves = map[string]int{}
	}
	for uri := range vs.early {
		vs.confirmed[uri] = struct{}{}
	}
	vs.early = nil
	vs.built = true
	p.Log.Debug("personal feed: viewer read", "feed", pf.feed.Rkey, "viewer", vs.did, "classified_likes", likes.Posts,
		"topics", len(vs.mass), "seen", len(vs.confirmed), "took", time.Since(start).Round(time.Millisecond))
}

// refreshProfile reads a viewer's interests again from their latest likes (and their tuning,
// if reading it failed before). The feed keeps using the old ones if that fails.
func (p *Personal) refreshProfile(pf *personalFeed, vs *viewerState) {
	now := p.now()
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	cfg := pf.cfg

	vs.mu.Lock()
	unknown := vs.tuningUnknown
	vs.mu.Unlock()
	var tuning Tuning
	var terr error
	if unknown {
		tuning, terr = p.readTuning(ctx, vs.did)
	}
	vs.mu.Lock()
	if unknown && terr == nil && !vs.tuningSet {
		vs.tuning, vs.tuningUnknown = tuning, false
	}
	key := vs.tuning.likeKey(cfg)
	vs.mu.Unlock()

	likes, err := p.Source.LikeProfile(ctx, vs.did, now.AddDate(0, 0, -key.lookbackDays), now, halfLifeDuration(key.halfLifeDays))
	vs.mu.Lock()
	defer vs.mu.Unlock()
	vs.refreshing = false
	if terr != nil {
		metricPersonalTuningErrors.Inc()
		p.Log.Warn("personal feed: reading a viewer's tuning failed", "feed", pf.feed.Rkey, "viewer", vs.did, "err", terr)
	}
	if err != nil {
		vs.refreshAfter = now.Add(time.Minute)
		p.Log.Warn("personal feed: refreshing a viewer's interests failed", "feed", pf.feed.Rkey, "viewer", vs.did, "err", err)
		return
	}
	if vs.tuning.likeKey(cfg) != key {
		vs.refreshAfter = time.Time{} // they changed it while this read: read again
		return
	}
	vs.mass, vs.likeCount, vs.massKey = likes.Mass, likes.Posts, key
	vs.liked = likes.Liked
	vs.refreshAfter = now.Add(profileTTL)
	if vs.tuningUnknown {
		vs.refreshAfter = now.Add(retryAfter)
	}
}

func halfLifeDuration(days float64) time.Duration {
	return time.Duration(days * 24 * float64(time.Hour))
}

// readTuning reads a viewer's saved tuning. A saved tuning that isn't valid (hand-edited, or
// from a version with other limits) counts as none, so it can't break their feed.
func (p *Personal) readTuning(ctx context.Context, did string) (Tuning, error) {
	if p.Tunings == nil {
		return Tuning{}, nil
	}
	t, err := p.Tunings.ViewerTuning(ctx, did)
	if err != nil {
		return Tuning{}, err
	}
	if err := t.check(); err != nil {
		p.Log.Error("personal feed: a viewer's saved tuning is not valid, ignoring it", "viewer", did, "err", err)
		return Tuning{}, nil
	}
	return t, nil
}

var errNoTuningStore = errors.New("tuning can't be saved: no store is configured")

// SetTuning saves how a viewer has adjusted their feed and applies it to every personal feed
// of theirs held in memory: their next request gets a feed assembled from the start with
// the new settings. Nothing is changed if saving fails.
func (p *Personal) SetTuning(ctx context.Context, did string, t Tuning) error {
	if err := t.check(); err != nil {
		return err
	}
	if p.Tunings == nil {
		return errNoTuningStore
	}
	if err := p.Tunings.SaveTuning(ctx, did, t); err != nil {
		return err
	}
	p.mu.Lock()
	type held struct {
		pf *personalFeed
		vs *viewerState
	}
	var states []held
	for rkey, pf := range p.feeds {
		if vs := p.viewers[viewerKey{rkey, did}]; vs != nil {
			states = append(states, held{pf, vs})
		}
	}
	p.mu.Unlock()
	for _, h := range states {
		p.applyTuning(ctx, h.pf, h.vs, t)
	}
	return nil
}

// applyTuning makes t the viewer's tuning. If it changes how fast their old likes fade,
// their likes are read again that way; if that read fails the feed uses the new settings with
// the old weighting until the next request retries it.
func (p *Personal) applyTuning(ctx context.Context, pf *personalFeed, vs *viewerState, t Tuning) {
	cfg := pf.cfg
	key := t.likeKey(cfg)
	vs.mu.Lock()
	vs.tuning, vs.tuningSet, vs.tuningUnknown = t, true, false
	vs.list = nil
	reread := vs.built && vs.massKey != key
	vs.mu.Unlock()
	if !reread {
		return
	}
	now := p.now()
	likes, err := p.Source.LikeProfile(ctx, vs.did, now.AddDate(0, 0, -key.lookbackDays), now, halfLifeDuration(key.halfLifeDays))
	vs.mu.Lock()
	defer vs.mu.Unlock()
	if err != nil {
		vs.refreshAfter = time.Time{}
		p.Log.Warn("personal feed: reading a viewer's likes for their new tuning failed", "feed", pf.feed.Rkey, "viewer", vs.did, "err", err)
		return
	}
	if vs.tuning.likeKey(cfg) != key {
		return // tuned again meanwhile; that call reads for itself
	}
	vs.mass, vs.likeCount, vs.massKey = likes.Mass, likes.Posts, key
	vs.liked = likes.Liked
	vs.refreshAfter = now.Add(profileTTL)
	vs.list = nil
}

// PreviewResult is what a draft tuning would do to a viewer's feed.
type PreviewResult struct {
	// Posts are the first posts of the feed, in order.
	Posts []Post
	// State is StatePersonal or StateGeneric, as for a page.
	State string
	// Interests are the topics the feed is built from with the draft, each with its share of
	// the slots, strongest first.
	Interests []TopicShare
}

// Preview is the first `limit` posts a viewer's feed would hold with the draft tuning, whether
// or not it is saved. It leaves out what they liked, their own posts, and what the draft hides,
// but not what they have already seen, and sends nothing to them and records nothing: it
// shows what the settings do, not what is left to read.
func (p *Personal) Preview(ctx context.Context, rkey, did string, draft Tuning, limit int) (PreviewResult, error) {
	pf := p.feeds[rkey]
	if pf == nil {
		return PreviewResult{}, errNotReady
	}
	if err := draft.check(); err != nil {
		return PreviewResult{}, err
	}
	now := p.now()
	pool := pf.poolFor(draft, now)
	if pool == nil {
		return PreviewResult{}, errNotReady
	}

	vs := p.viewer(pf, did)
	select {
	case <-vs.ready:
	case <-ctx.Done():
		return PreviewResult{}, ctx.Err()
	}
	vs.mu.Lock()
	if vs.err != nil {
		err := vs.err
		vs.mu.Unlock()
		return PreviewResult{}, err
	}
	mass, likeCount, liked, massKey := vs.mass, vs.likeCount, vs.liked, vs.massKey
	vs.mu.Unlock()

	cfg := draft.Config(pf.cfg)
	weights := pf.feed.Ranking.Weights
	if key := draft.likeKey(pf.cfg); key != massKey {
		likes, err := p.Source.LikeProfile(ctx, did, now.AddDate(0, 0, -key.lookbackDays), now, halfLifeDuration(key.halfLifeDays))
		if err != nil {
			return PreviewResult{}, err
		}
		mass, likeCount, liked = likes.Mass, likes.Posts, likes.Liked
	}
	prof, state := ProfileFor(cfg, draft, mass, likeCount, pool, now)
	skip := func(post Post) bool {
		if _, ok := liked[post.URI]; ok {
			return true
		}
		return post.DID == did || draft.Excludes(post, cfg, now) || belowMinEngagement(post, cfg, weights)
	}
	limit = min(max(limit, 1), cfg.ListSize)
	return PreviewResult{Posts: Assemble(prof, pool, skip, limit, *cfg.AuthorGap), State: state, Interests: prof.Topics}, nil
}

// NoteInteractions records that Bluesky reported the viewer as having met these posts (seen,
// liked, "show less", ...) in any of our feeds, so no personal feed shows them again.
func (p *Personal) NoteInteractions(viewer string, uris []string) {
	if viewer == "" || len(uris) == 0 {
		return
	}
	p.mu.Lock()
	var states []*viewerState
	for rkey := range p.feeds {
		if vs := p.viewers[viewerKey{rkey, viewer}]; vs != nil {
			states = append(states, vs)
		}
	}
	p.mu.Unlock()
	for _, vs := range states {
		vs.mu.Lock()
		into := vs.confirmed
		if !vs.built { // still being read: merged in when that finishes
			into = vs.early
		}
		for _, u := range uris {
			into[u] = struct{}{}
		}
		vs.mu.Unlock()
	}
}
