package feedgen

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"maps"
	"net/http"
	"net/http/httptest"
	"net/url"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"gopkg.in/yaml.v3"

	"github.com/haileyok/topic-feed/internal/taxonomy"
)

// topicPosts returns n posts of one subtopic, best first, each by its own author.
func topicPosts(topic string, n int) []Post {
	tag := strings.ReplaceAll(topic, "/", "-")
	out := make([]Post, n)
	for i := range out {
		out[i] = Post{
			URI: fmt.Sprintf("at://did:plc:%s-%d/app.bsky.feed.post/1", tag, i), DID: fmt.Sprintf("did:plc:%s-%d", tag, i),
			IndexedAt: now.Add(-time.Duration(i+1) * time.Minute), Likes: uint64(n - i),
			TopPath: topic, TopPathP: 0.9, TopPaths: []string{topic}, TopPs: []float32{0.9},
		}
	}
	return out
}

func noSkip(Post) bool { return false }

func topicCounts(ps []Post) map[string]int {
	out := map[string]int{}
	for _, p := range ps {
		out[p.TopPath]++
	}
	return out
}

func TestNewProfile(t *testing.T) {
	p := NewProfile(map[string]float64{
		"technology/ai": 3, "animals_nature/cats": 1, "unclear": 100, "humor/shitposts": 0, "x/neg": -2,
	}, 12, 5, now)
	if len(p.Topics) != 2 || p.Topics[0].Path != "technology/ai" || p.Topics[0].Share != 0.75 ||
		p.Topics[1].Path != "animals_nature/cats" || p.Topics[1].Share != 0.25 {
		t.Errorf("unclear, empty and negative interests should be left out: %+v", p.Topics)
	}
	if p.Likes != 12 || !p.BuiltAt.Equal(now) {
		t.Errorf("%+v", p)
	}

	// Only the strongest interests are kept, and the shares are of those.
	p = NewProfile(map[string]float64{"a/x": 4, "b/x": 3, "c/x": 2, "d/x": 1}, 10, 2, now)
	if len(p.Topics) != 2 || p.Topics[0].Share != 4.0/7 || p.Topics[1].Share != 3.0/7 {
		t.Errorf("top two: %+v", p.Topics)
	}
	// Equal interests come out in path order, so the same likes always give the same profile.
	for range 20 {
		p = NewProfile(map[string]float64{"c/x": 1, "a/x": 1, "b/x": 1}, 3, 3, now)
		if p.Topics[0].Path != "a/x" || p.Topics[1].Path != "b/x" || p.Topics[2].Path != "c/x" {
			t.Fatalf("ties: %+v", p.Topics)
		}
	}
	total := 0.0
	for _, s := range p.Topics {
		total += s.Share
	}
	if total < 0.999999 || total > 1.000001 {
		t.Errorf("shares add up to %v", total)
	}

	if (NewProfile(nil, 0, 5, now)).Personalized(1) || NewProfile(map[string]float64{"a/x": 1}, 4, 5, now).Personalized(5) ||
		!NewProfile(map[string]float64{"a/x": 1}, 5, 5, now).Personalized(5) {
		t.Error("Personalized needs enough likes and at least one interest")
	}
}

func TestGenericProfile(t *testing.T) {
	pool := map[string][]Post{
		"a/big": topicPosts("a/big", 50), "b/mid": topicPosts("b/mid", 30), "c/small": topicPosts("c/small", 5),
		"unclear": topicPosts("unclear", 60), "d/mid": topicPosts("d/mid", 30),
	}
	p := GenericProfile(pool, 10, now)
	var got []string
	for _, s := range p.Topics {
		got = append(got, s.Path)
		if s.Share != 1.0/3 {
			t.Errorf("%s: share %v, want an even split", s.Path, s.Share)
		}
	}
	if !slices.Equal(got, []string{"a/big", "b/mid", "d/mid"}) {
		t.Errorf("busy subtopics only, not unclear: %v", got)
	}
	if p.Likes != 0 || p.Personalized(1) {
		t.Error("a generic profile rests on no likes")
	}
	if got := GenericProfile(pool, 2, now); len(got.Topics) != 2 {
		t.Errorf("capped at 2 topics: %+v", got.Topics)
	}
}

func TestAssembleFollowsTheShares(t *testing.T) {
	pool := map[string][]Post{"a/x": topicPosts("a/x", 100), "b/x": topicPosts("b/x", 100), "c/x": topicPosts("c/x", 100)}
	prof := Profile{Topics: []TopicShare{{"a/x", 0.5}, {"b/x", 0.3}, {"c/x", 0.2}}}
	out := Assemble(prof, pool, noSkip, 100, 0)
	if len(out) != 100 {
		t.Fatalf("%d posts", len(out))
	}
	c := topicCounts(out)
	if abs(c["a/x"]-50) > 1 || abs(c["b/x"]-30) > 1 || abs(c["c/x"]-20) > 1 {
		t.Errorf("slots %v, want about 50/30/20", c)
	}
	// The interests are mixed, not taken one after another: all three show up early.
	if c := topicCounts(out[:6]); len(c) != 3 {
		t.Errorf("first six slots: %v", c)
	}
	// Within a subtopic the best-ranked post comes first.
	var last = -1
	for _, p := range out {
		if p.TopPath != "a/x" {
			continue
		}
		var i int
		fmt.Sscanf(p.DID, "did:plc:a-x-%d", &i)
		if i != last+1 {
			t.Fatalf("a/x posts out of rank order at %s", p.URI)
		}
		last = i
	}
}

func abs(n int) int {
	if n < 0 {
		return -n
	}
	return n
}

func TestAssembleSkipsAndNeverRepeats(t *testing.T) {
	pool := map[string][]Post{"a/x": topicPosts("a/x", 40), "b/x": topicPosts("b/x", 40)}
	prof := Profile{Topics: []TopicShare{{"a/x", 0.6}, {"b/x", 0.4}}}
	skip := func(p Post) bool { return strings.HasSuffix(p.DID, "-3") || strings.HasSuffix(p.DID, "-7") }
	out := Assemble(prof, pool, skip, 80, 0)
	seen := map[string]bool{}
	for _, p := range out {
		if skip(p) {
			t.Errorf("%s was left out but shown", p.URI)
		}
		if seen[p.URI] {
			t.Errorf("%s twice", p.URI)
		}
		seen[p.URI] = true
	}
	if len(out) != 76 { // 80 posts, 4 skipped
		t.Errorf("%d posts, want every one not skipped", len(out))
	}
}

func TestAssembleGivesAnExhaustedTopicsSlotsAway(t *testing.T) {
	pool := map[string][]Post{"a/x": topicPosts("a/x", 3), "b/x": topicPosts("b/x", 50)}
	prof := Profile{Topics: []TopicShare{{"a/x", 0.9}, {"b/x", 0.1}}}
	out := Assemble(prof, pool, noSkip, 20, 0)
	if c := topicCounts(out); len(out) != 20 || c["a/x"] != 3 || c["b/x"] != 17 {
		t.Errorf("%d posts, %v: a/x has only 3 posts to show", len(out), c)
	}
	// Asking for more than there is returns what there is.
	if out := Assemble(prof, pool, noSkip, 500, 0); len(out) != 53 {
		t.Errorf("%d posts, want all 53", len(out))
	}
	// A subtopic with no posts at all doesn't hold back the others' shares.
	prof = Profile{Topics: []TopicShare{{"missing/x", 0.5}, {"b/x", 0.5}}}
	if out := Assemble(prof, pool, noSkip, 10, 0); len(out) != 10 {
		t.Errorf("%d posts", len(out))
	}
	if out := Assemble(Profile{}, pool, noSkip, 10, 0); len(out) != 0 {
		t.Errorf("no interests, no posts: %d", len(out))
	}
}

func TestAssembleKeepsAuthorsApart(t *testing.T) {
	a, b := topicPosts("a/x", 20), topicPosts("b/x", 20)
	for i := 0; i < 6; i++ { // one author wrote the six best posts of both subtopics
		a[i].DID, b[i].DID = "did:plc:loud", "did:plc:loud"
	}
	prof := Profile{Topics: []TopicShare{{"a/x", 0.5}, {"b/x", 0.5}}}
	out := Assemble(prof, map[string][]Post{"a/x": a, "b/x": b}, noSkip, 40, 5)
	var slots []int
	for i, p := range out {
		if p.DID == "did:plc:loud" {
			slots = append(slots, i)
		}
	}
	if len(slots) != 12 {
		t.Fatalf("every post is still shown: %d of 12 by the loud author", len(slots))
	}
	for i := 1; i < 6; i++ {
		if slots[i]-slots[i-1] < 5 {
			t.Fatalf("loud author's slots %v: want the first posts at least 5 apart", slots)
		}
	}

	// Only one author left: their posts are shown anyway, rather than nothing.
	solo := topicPosts("a/x", 5)
	for i := range solo {
		solo[i].DID = "did:plc:solo"
	}
	if out := Assemble(Profile{Topics: []TopicShare{{"a/x", 1}}}, map[string][]Post{"a/x": solo}, noSkip, 10, 5); len(out) != 5 {
		t.Errorf("%d posts", len(out))
	}
}

func TestAssembleIsDeterministic(t *testing.T) {
	pool := map[string][]Post{"a/x": topicPosts("a/x", 30), "b/x": topicPosts("b/x", 30), "c/x": topicPosts("c/x", 30)}
	prof := Profile{Topics: []TopicShare{{"a/x", 1.0 / 3}, {"b/x", 1.0 / 3}, {"c/x", 1.0 / 3}}}
	first := Assemble(prof, pool, noSkip, 60, 4)
	for range 20 {
		if !slices.EqualFunc(first, Assemble(prof, pool, noSkip, 60, 4), func(a, b Post) bool { return a.URI == b.URI }) {
			t.Fatal("the same inputs gave a different feed")
		}
	}
}

func TestRankPool(t *testing.T) {
	ps := topicPosts("a/x", 10)
	for i := range ps {
		ps[i].DID = "did:plc:same" // an author gap must not reorder or drop anything here
		ps[i].Likes = uint64(i * 100)
	}
	r := DefaultRanking
	r.FreshEvery = 0
	out := RankPool(map[string][]Post{"a/x": ps}, r, now)["a/x"]
	if len(out) != 10 || out[0].URI != ps[9].URI {
		t.Errorf("most-liked first, every post kept: %d posts, first %s", len(out), out[0].URI)
	}
	if ps[0].Score != 0 {
		t.Error("the input was modified")
	}
}

// --- the service: viewers, paging, and what they have seen -----------------------------

type fakeSource struct {
	mu       sync.Mutex
	likes    LikeData
	likesErr error
	seen     SeenData
	pool     map[string][]Post
	poolErr  error
	// poolQueries is every pool asked for.
	poolQueries []PoolQuery
	delay       time.Duration // how long reading a viewer takes
	calls       int
	track       *readingAtOnce
	// likesFor, if set, gives the likes for a half-life instead of `likes`.
	likesFor func(hl time.Duration) LikeData
	// hold, if set, can make a likes read wait: it returns a channel to wait on (nil: don't).
	hold func(hl time.Duration) chan struct{}
	hls  []time.Duration // the half-life of each likes read
	// lookbacks is how far back each likes read went.
	lookbacks []time.Duration
}

func (f *fakeSource) lookbackDurations() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.lookbacks)
}

func (f *fakeSource) halfLives() []time.Duration {
	f.mu.Lock()
	defer f.mu.Unlock()
	return slices.Clone(f.hls)
}

func (f *fakeSource) LikeProfile(ctx context.Context, did string, since, at time.Time, hl time.Duration) (LikeData, error) {
	f.mu.Lock()
	f.calls++
	f.hls = append(f.hls, hl)
	f.lookbacks = append(f.lookbacks, at.Sub(since))
	delay, err, d, track := f.delay, f.likesErr, f.likes, f.track
	if f.likesFor != nil {
		d = f.likesFor(hl)
	}
	hold := f.hold
	f.mu.Unlock()
	if hold != nil {
		if ch := hold(hl); ch != nil {
			select {
			case <-ch:
			case <-ctx.Done():
				return LikeData{}, ctx.Err()
			}
		}
	}
	if track != nil {
		track.enter()
		defer track.leave()
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return LikeData{}, ctx.Err()
		}
	}
	return LikeData{Mass: maps.Clone(d.Mass), Posts: d.Posts, Liked: maps.Clone(d.Liked)}, err
}

func (f *fakeSource) ViewerSeen(ctx context.Context, did string, since time.Time) (SeenData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return SeenData{Confirmed: maps.Clone(f.seen.Confirmed), Serves: maps.Clone(f.seen.Serves)}, nil
}

func (f *fakeSource) TopicPool(_ context.Context, q PoolQuery) (map[string][]Post, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.poolQueries = append(f.poolQueries, q)
	return f.pool, f.poolErr
}

// readingAtOnce tracks how many viewers are being read at the same moment.
type readingAtOnce struct {
	mu       sync.Mutex
	now, max int
}

func (r *readingAtOnce) enter() {
	r.mu.Lock()
	r.now++
	r.max = max(r.max, r.now)
	r.mu.Unlock()
}

func (r *readingAtOnce) leave() {
	r.mu.Lock()
	r.now--
	r.mu.Unlock()
}

func (r *readingAtOnce) peak() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.max
}

func (f *fakeSource) likeCalls() int {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.calls
}

type servedSink struct {
	mu   sync.Mutex
	rows []ServedRow
}

func (s *servedSink) Add(r []ServedRow) int {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.rows = append(s.rows, r...)
	return 0
}

type testClock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *testClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *testClock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

const (
	viewerDID  = "did:plc:viewer"
	welcomeURI = "at://did:plc:owner/app.bsky.feed.post/welcome"
)

// aiLover has liked mostly AI posts and some cats: well past the default 5 likes needed.
func aiLover() LikeData {
	return LikeData{Mass: map[string]float64{"technology/ai": 30, "animals_nature/cats": 10}, Posts: 40, Liked: map[string]struct{}{}}
}

func twoTopicPool(n int) map[string][]Post {
	return map[string][]Post{"technology/ai": topicPosts("technology/ai", n), "animals_nature/cats": topicPosts("animals_nature/cats", n)}
}

func newPersonalFor(t *testing.T, src *fakeSource, tweak func(*PersonalConfig)) (*Personal, *testClock, *servedSink) {
	t.Helper()
	cfg := PersonalConfigDefaults()
	// Most of these tests are about other rules and use posts nobody has reacted to; the minimum
	// engagement has tests of its own, which turn it on.
	noMinimum := 0.0
	cfg.MinEngagement = &noMinimum
	if tweak != nil {
		tweak(&cfg)
	}
	feed := Feed{Rkey: "for-you", DisplayName: "For you", Ranking: DefaultRanking, Personal: &cfg}
	p := NewPersonal([]Feed{feed}, src, slog.New(slog.NewTextHandler(io.Discard, nil)))
	clock := &testClock{t: now}
	p.Now = clock.Now
	sink := &servedSink{}
	p.Served = sink
	p.Welcome = welcomeURI
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	p.Start(ctx)
	return p, clock, sink
}

func page(t *testing.T, p *Personal, viewer, cursor string, limit int) PersonalPage {
	t.Helper()
	pg, err := p.Page(context.Background(), "for-you", viewer, cursor, limit)
	if err != nil {
		t.Fatalf("page: %v", err)
	}
	return pg
}

func itemURIs(pg PersonalPage) []string {
	out := make([]string, len(pg.Items))
	for i, it := range pg.Items {
		out[i] = it.URI
	}
	return out
}

// waitPersonal asks until the viewer's feed is ready (not the welcome post).
func waitPersonal(t *testing.T, p *Personal, viewer string, limit int) PersonalPage {
	t.Helper()
	for range 200 {
		if pg := page(t, p, viewer, "", limit); pg.State != "welcome" {
			return pg
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("the viewer's feed never became ready")
	return PersonalPage{}
}

func TestPersonalShowsWelcomeWhileBuildingThenTheFeed(t *testing.T) {
	src := &fakeSource{likes: aiLover(), pool: twoTopicPool(30), delay: 300 * time.Millisecond}
	p, _, _ := newPersonalFor(t, src, nil)
	p.Budget = 30 * time.Millisecond

	pg := page(t, p, viewerDID, "", 20)
	if pg.State != "welcome" || len(pg.Items) != 1 || pg.Items[0].URI != welcomeURI || pg.Cursor != "" {
		t.Fatalf("first request while the viewer is read: %+v", pg)
	}
	pg = waitPersonal(t, p, viewerDID, 20)
	if pg.State != "personal" || len(pg.Items) != 20 {
		t.Fatalf("once read: %+v", pg)
	}
	for _, u := range itemURIs(pg) {
		if u == welcomeURI {
			t.Error("the welcome post stays after the feed is ready")
		}
	}
	if n := src.likeCalls(); n != 1 {
		t.Errorf("the viewer was read %d times, want once however many requests came", n)
	}
}

func TestPersonalFastBuildSkipsWelcome(t *testing.T) {
	src := &fakeSource{likes: aiLover(), pool: twoTopicPool(30)}
	p, _, _ := newPersonalFor(t, src, nil)
	if pg := page(t, p, viewerDID, "", 10); pg.State != "personal" || len(pg.Items) != 10 {
		t.Errorf("a viewer read within the budget gets their feed straight away: %+v", pg)
	}
}

func TestPersonalLimitsHowManyViewersAreReadAtOnce(t *testing.T) {
	track := &readingAtOnce{}
	src := &fakeSource{likes: aiLover(), pool: twoTopicPool(10), delay: 40 * time.Millisecond, track: track}
	p, _, _ := newPersonalFor(t, src, nil)
	p.Budget = time.Millisecond
	// A feed gets shared and forty new viewers open it at the same moment.
	const viewers = 40
	for i := range viewers {
		if pg := page(t, p, fmt.Sprintf("did:plc:v%d", i), "", 5); pg.State != "welcome" {
			t.Fatalf("viewer %d: %+v", i, pg)
		}
	}
	// Every one of them still gets their feed, the database seeing at most a few at a time.
	for i := range viewers {
		if pg := waitPersonal(t, p, fmt.Sprintf("did:plc:v%d", i), 5); len(pg.Items) != 5 {
			t.Fatalf("viewer %d: %+v", i, pg)
		}
	}
	if peak := track.peak(); peak > maxViewerReads || peak < 2 {
		t.Errorf("%d viewers were read at once, want between 2 and %d", peak, maxViewerReads)
	}
}

func TestPersonalWelcomeForAnonymousAndWithoutOne(t *testing.T) {
	src := &fakeSource{likes: aiLover(), pool: twoTopicPool(5)}
	p, _, _ := newPersonalFor(t, src, nil)
	if pg := page(t, p, "", "", 10); pg.State != "welcome" || len(pg.Items) != 1 || pg.Items[0].URI != welcomeURI {
		t.Errorf("no viewer: %+v", pg)
	}
	p.Welcome = ""
	pg := page(t, p, "", "", 10)
	if pg.State != "welcome" || pg.Items == nil || len(pg.Items) != 0 {
		t.Errorf("no welcome post set: an empty feed, not nil: %+v", pg)
	}
}

func TestPersonalNotReadyWithoutPosts(t *testing.T) {
	src := &fakeSource{likes: aiLover(), poolErr: fmt.Errorf("clickhouse is down")}
	p, _, _ := newPersonalFor(t, src, nil)
	if _, err := p.Page(context.Background(), "for-you", viewerDID, "", 10); err != errNotReady {
		t.Errorf("err %v, want errNotReady", err)
	}
	if _, _, ok := p.Status("for-you"); ok {
		t.Error("status says the posts were read")
	}
}

func TestPersonalPagingCoversTheFeedOnceAndStoresWhatWasServed(t *testing.T) {
	src := &fakeSource{likes: aiLover(), pool: twoTopicPool(30)}
	p, _, sink := newPersonalFor(t, src, nil)

	var all []string
	cursor := ""
	pages := 0
	for {
		pg := page(t, p, viewerDID, cursor, 7)
		all = append(all, itemURIs(pg)...)
		pages++
		if pg.Cursor == "" {
			break
		}
		cursor = pg.Cursor
		if pages > 30 {
			t.Fatal("paging never ended")
		}
	}
	seen := map[string]bool{}
	for _, u := range all {
		if seen[u] {
			t.Fatalf("%s on two pages", u)
		}
		seen[u] = true
	}
	if len(all) != 60 {
		t.Errorf("%d posts over %d pages, want all 60", len(all), pages)
	}
	sink.mu.Lock()
	defer sink.mu.Unlock()
	if len(sink.rows) != len(all) {
		t.Fatalf("%d rows stored for %d posts served", len(sink.rows), len(all))
	}
	for i, r := range sink.rows {
		if r.ViewerDID != viewerDID || r.Feed != "for-you" || r.URI != all[i] || !r.ServedAt.Equal(now) {
			t.Fatalf("row %d: %+v", i, r)
		}
	}
}

func TestPersonalFollowsTheViewersInterests(t *testing.T) {
	src := &fakeSource{likes: aiLover(), pool: twoTopicPool(100)}
	p, _, _ := newPersonalFor(t, src, func(c *PersonalConfig) { *c.AuthorGap = 0 })
	// The viewer liked AI three times as much as cats.
	pg := page(t, p, viewerDID, "", 40)
	var ai, cats int
	for _, it := range pg.Items {
		switch {
		case strings.Contains(it.URI, "technology-ai"):
			ai++
		case strings.Contains(it.URI, "animals_nature-cats"), strings.Contains(it.URI, "animals-nature-cats"):
			cats++
		}
	}
	if ai+cats != 40 || ai != 30 || cats != 10 {
		t.Errorf("%d AI and %d cat posts in 40 slots, want 30 and 10", ai, cats)
	}
}

func TestPersonalLeavesOutLikedOwnAndSeenPosts(t *testing.T) {
	pool := twoTopicPool(20)
	likes := aiLover()
	liked := pool["technology/ai"][0].URI
	likes.Liked[liked] = struct{}{}
	own := pool["technology/ai"][1]
	pool["technology/ai"][1].DID = viewerDID
	seen := pool["animals_nature/cats"][0].URI
	src := &fakeSource{likes: likes, pool: pool, seen: SeenData{Confirmed: map[string]struct{}{seen: {}}}}
	p, _, _ := newPersonalFor(t, src, nil)

	got := page(t, p, viewerDID, "", 100)
	if len(got.Items) != 37 {
		t.Errorf("%d posts, want the 40 less the three left out", len(got.Items))
	}
	for _, u := range itemURIs(got) {
		if u == liked || u == own.URI || u == seen {
			t.Errorf("%s should have been left out", u)
		}
	}
}

func TestPersonalRepeatsAPostOnlyAsOftenAsMaxServesAllows(t *testing.T) {
	for _, tc := range []struct {
		maxServes int
		want      []int // posts sent on each first-page request
	}{
		{1, []int{12, 0, 0}},
		{2, []int{12, 12, 0}},
		{3, []int{12, 12, 12, 0}},
	} {
		src := &fakeSource{likes: aiLover(), pool: twoTopicPool(6)}
		p, _, _ := newPersonalFor(t, src, func(c *PersonalConfig) { c.MaxServes = tc.maxServes })
		for i, want := range tc.want {
			if got := len(page(t, p, viewerDID, "", 100).Items); got != want {
				t.Errorf("max_serves %d, request %d: %d posts, want %d", tc.maxServes, i+1, got, want)
			}
		}
	}
}

func TestPersonalSeenPostsAreNotShownAgain(t *testing.T) {
	src := &fakeSource{likes: aiLover(), pool: twoTopicPool(6)}
	p, _, _ := newPersonalFor(t, src, nil)
	first := itemURIs(page(t, p, viewerDID, "", 100))
	if len(first) != 12 {
		t.Fatalf("%d posts", len(first))
	}
	// Bluesky reports the first three as seen (in any of our feeds).
	p.NoteInteractions(viewerDID, first[:3])
	second := itemURIs(page(t, p, viewerDID, "", 100))
	if len(second) != 9 {
		t.Fatalf("%d posts after three were seen, want the other 9", len(second))
	}
	for _, u := range second {
		if slices.Contains(first[:3], u) {
			t.Errorf("%s was reported seen but shown again", u)
		}
	}
	// Views are the viewer's own: another viewer's don't leave anything out for this one.
	other := "did:plc:other"
	if got := len(page(t, p, other, "", 100).Items); got != 12 {
		t.Fatalf("another viewer's first feed: %d posts", got)
	}
	p.NoteInteractions(other, first[3:6])
	if got := len(page(t, p, other, "", 100).Items); got != 9 {
		t.Errorf("another viewer got %d posts after three were reported seen, want 9", got)
	}
	p.mu.Lock()
	mine := p.viewers[viewerKey{"for-you", viewerDID}]
	p.mu.Unlock()
	mine.mu.Lock()
	defer mine.mu.Unlock()
	if len(mine.confirmed) != 3 {
		t.Errorf("the first viewer has %d posts marked seen, want only their own 3", len(mine.confirmed))
	}
}

func TestPersonalCountsViewsReportedWhileTheViewerIsBeingRead(t *testing.T) {
	pool := twoTopicPool(6)
	src := &fakeSource{likes: aiLover(), pool: pool, delay: 200 * time.Millisecond}
	p, _, _ := newPersonalFor(t, src, nil)
	p.Budget = 20 * time.Millisecond
	if pg := page(t, p, viewerDID, "", 100); pg.State != "welcome" {
		t.Fatalf("%+v", pg)
	}
	target := pool["technology/ai"][0].URI
	p.NoteInteractions(viewerDID, []string{target}) // arrives before the database read finishes
	for _, u := range itemURIs(waitPersonal(t, p, viewerDID, 100)) {
		if u == target {
			t.Error("a view reported during the first read was lost")
		}
	}
}

func TestPersonalRememberedServesCarryOver(t *testing.T) {
	pool := twoTopicPool(6)
	// A previous run sent every AI post twice and one cat post once: with max_serves 2 the
	// AI posts are done, and the cat post has one showing left.
	serves := map[string]int{}
	for _, p := range pool["technology/ai"] {
		serves[p.URI] = 2
	}
	serves[pool["animals_nature/cats"][0].URI] = 1
	src := &fakeSource{likes: aiLover(), pool: pool, seen: SeenData{Serves: serves}}
	p, _, _ := newPersonalFor(t, src, nil)
	if got := page(t, p, viewerDID, "", 100); len(got.Items) != 6 {
		t.Errorf("%d posts, want the six cat posts", len(got.Items))
	}
}

func TestPersonalStaleCursorStartsAgainWithoutRepeating(t *testing.T) {
	src := &fakeSource{likes: aiLover(), pool: twoTopicPool(30)}
	p, _, _ := newPersonalFor(t, src, func(c *PersonalConfig) { c.MaxServes = 1 })
	one := page(t, p, viewerDID, "", 10)
	if one.Cursor == "" {
		t.Fatal("no cursor")
	}
	// The viewer pulls to refresh on another device, which replaces their feed...
	two := page(t, p, viewerDID, "", 10)
	// ...and then the first device asks for its next page.
	three := page(t, p, viewerDID, one.Cursor, 10)
	shown := map[string]bool{}
	for _, pg := range []PersonalPage{one, two, three} {
		for _, u := range itemURIs(pg) {
			if shown[u] {
				t.Fatalf("%s shown twice", u)
			}
			shown[u] = true
		}
	}
	if len(three.Items) != 10 {
		t.Errorf("the stale cursor should still give a page: %d posts", len(three.Items))
	}
	// Nothing was skipped either: in each subtopic, what was shown is the best posts, in order,
	// with no gap (a stale cursor must not continue at its old position in a new feed).
	rank := map[string][]int{}
	for u := range shown {
		topic := "ai"
		if strings.Contains(u, "cats") {
			topic = "cats"
		}
		var i int
		fmt.Sscanf(u[strings.LastIndex(u, "-")+1:], "%d", &i)
		rank[topic] = append(rank[topic], i)
	}
	for topic, ranks := range rank {
		slices.Sort(ranks)
		for want, got := range ranks {
			if got != want {
				t.Fatalf("%s posts shown have ranks %v: the best posts, with none skipped, are 0..%d", topic, ranks, len(ranks)-1)
			}
		}
	}
}

func TestPersonalBadCursor(t *testing.T) {
	src := &fakeSource{likes: aiLover(), pool: twoTopicPool(5)}
	p, _, _ := newPersonalFor(t, src, nil)
	if _, err := p.Page(context.Background(), "for-you", viewerDID, "garbage", 10); err != errBadCursor {
		t.Errorf("err %v", err)
	}
}

func TestPersonalGenericMixForViewersWhoHaveLikedLittle(t *testing.T) {
	few := LikeData{Mass: map[string]float64{"technology/ai": 2}, Posts: 2, Liked: map[string]struct{}{}}
	pool := map[string][]Post{}
	for _, tp := range []string{"a/x", "b/x", "c/x"} {
		pool[tp] = topicPosts(tp, 40)
	}
	src := &fakeSource{likes: few, pool: pool}
	p, _, _ := newPersonalFor(t, src, func(c *PersonalConfig) { *c.AuthorGap = 0 })
	pg := page(t, p, viewerDID, "", 30)
	if pg.State != "generic" {
		t.Fatalf("state %q", pg.State)
	}
	// Two likes aren't enough to go on: an even mix of the busy subtopics instead.
	if c := topicCounts(postsOf(pool, itemURIs(pg))); c["a/x"] != 10 || c["b/x"] != 10 || c["c/x"] != 10 {
		t.Errorf("%v", c)
	}
}

func postsOf(pool map[string][]Post, uris []string) []Post {
	idx := map[string]Post{}
	for _, ps := range pool {
		for _, p := range ps {
			idx[p.URI] = p
		}
	}
	out := make([]Post, len(uris))
	for i, u := range uris {
		out[i] = idx[u]
	}
	return out
}

func TestPersonalRecoversFromAFailedRead(t *testing.T) {
	src := &fakeSource{likes: aiLover(), likesErr: fmt.Errorf("clickhouse hiccup"), pool: twoTopicPool(10)}
	p, clock, _ := newPersonalFor(t, src, nil)
	if pg := page(t, p, viewerDID, "", 10); pg.State != "welcome" {
		t.Fatalf("a failed read shows the welcome post: %+v", pg)
	}
	src.mu.Lock()
	src.likesErr = nil
	src.mu.Unlock()
	if pg := page(t, p, viewerDID, "", 10); pg.State != "welcome" {
		t.Errorf("no retry for a few seconds, to not hammer a struggling database: %+v", pg)
	}
	clock.Advance(retryAfter + time.Second)
	if pg := page(t, p, viewerDID, "", 10); pg.State != "personal" || len(pg.Items) != 10 {
		t.Errorf("after the retry delay: %+v", pg)
	}
}

func TestPersonalReadsInterestsAgainInTheBackground(t *testing.T) {
	src := &fakeSource{likes: aiLover(), pool: twoTopicPool(10)}
	p, clock, _ := newPersonalFor(t, src, nil)
	page(t, p, viewerDID, "", 5)
	page(t, p, viewerDID, "", 5)
	if n := src.likeCalls(); n != 1 {
		t.Fatalf("%d reads, want 1", n)
	}
	clock.Advance(profileTTL + time.Minute)
	page(t, p, viewerDID, "", 5)
	for range 100 {
		if src.likeCalls() == 2 {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Errorf("%d reads, want the interests read again once they were old", src.likeCalls())
}

func TestForgetIdleViewers(t *testing.T) {
	src := &fakeSource{likes: aiLover(), pool: twoTopicPool(5)}
	p, clock, _ := newPersonalFor(t, src, nil)
	page(t, p, viewerDID, "", 5)
	clock.Advance(viewerIdle - time.Minute)
	page(t, p, "did:plc:active", "", 5)
	clock.Advance(2 * time.Minute)
	p.forgetIdle()
	p.mu.Lock()
	defer p.mu.Unlock()
	if len(p.viewers) != 1 {
		t.Errorf("%d viewers kept, want only the one who came back", len(p.viewers))
	}
}

func TestPersonalConfig(t *testing.T) {
	tax, err := taxonomy.Load("../../taxonomy/v1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	paths := TaxonomyPaths(tax)
	var c Config
	if err := yaml.Unmarshal([]byte(`feeds:
  - rkey: for-you
    display_name: For you
    personal: {}
  - rkey: custom
    display_name: Custom
    personal: {max_serves: 1, author_gap: 0, per_topic: 50}
    ranking: {gravity: 1.2}
`), &c); err != nil {
		t.Fatal(err)
	}
	if err := c.Validate(paths); err != nil {
		t.Fatalf("personal feeds need no paths or min_prob: %v", err)
	}
	d := c.Feeds[0].Personal
	if d.LookbackDays != 30 || d.HalfLifeDays != 7 || d.MinLikes != 5 || d.Topics != 20 || d.WindowHours != 24 ||
		d.PerTopic != 200 || d.MinTopicProb != 0.5 || d.MaxServes != 2 || d.ListSize != 300 || d.AuthorGap == nil || *d.AuthorGap != 10 {
		t.Errorf("defaults: %+v", d)
	}
	x := c.Feeds[1].Personal
	if x.MaxServes != 1 || x.PerTopic != 50 || x.AuthorGap == nil || *x.AuthorGap != 0 || x.Topics != 20 {
		t.Errorf("an explicit author_gap of 0 means no gap, not the default: %+v", x)
	}
	if c.Feeds[1].Ranking.Gravity != 1.2 {
		t.Errorf("ranking: %+v", c.Feeds[1].Ranking)
	}

	bad := func(name string, f func(*Feed)) {
		cfg := PersonalConfigDefaults()
		feed := Feed{Rkey: "ok", DisplayName: "x", Ranking: DefaultRanking, Personal: &cfg}
		f(&feed)
		if err := (&Config{Feeds: []Feed{feed}}).Validate(paths); err == nil {
			t.Errorf("accepted %s", name)
		}
	}
	bad("paths on a personal feed", func(f *Feed) { f.Paths = []string{"technology/ai"} })
	bad("min_prob on a personal feed", func(f *Feed) { f.MinProb = 0.5 })
	bad("exclude on a personal feed", func(f *Feed) { f.Exclude = map[string]float32{"adult_content": 0.2} })
	bad("zero topics", func(f *Feed) { f.Personal.Topics = 0 })
	bad("a negative half-life", func(f *Feed) { f.Personal.HalfLifeDays = -1 })
	bad("min_topic_prob above 1", func(f *Feed) { f.Personal.MinTopicProb = 2 })
	bad("a negative author gap", func(f *Feed) { n := -1; f.Personal.AuthorGap = &n })
	bad("negative ranking", func(f *Feed) { f.Ranking.Gravity = -1 })
	bad("too many max_serves", func(f *Feed) { f.Personal.MaxServes = 99 })
	bad("a negative min_engagement", func(f *Feed) { v := -1.0; f.Personal.MinEngagement = &v })
	bad("a min_engagement above the most", func(f *Feed) { v := MaxMinEngagement + 1; f.Personal.MinEngagement = &v })

	// A feed with paths still needs them.
	if err := (&Config{Feeds: []Feed{{Rkey: "ok", DisplayName: "x", Ranking: DefaultRanking}}}).Validate(paths); err == nil {
		t.Error("a feed with neither paths nor personal was accepted")
	}
}

// A personal feed has no topic paths, so the shared rebuild must never try to build it.
type refusesPersonal struct {
	t     *testing.T
	built []string
	mu    sync.Mutex
}

func (b *refusesPersonal) Build(_ context.Context, f Feed, _ time.Time, _ int) ([]Post, Removed, error) {
	b.mu.Lock()
	b.built = append(b.built, f.Rkey)
	b.mu.Unlock()
	if f.Personal != nil {
		b.t.Errorf("the shared rebuild tried to build the personal feed %q", f.Rkey)
	}
	return posts(3), Removed{}, nil
}

func TestSharedRebuildSkipsPersonalFeeds(t *testing.T) {
	cfg := PersonalConfigDefaults()
	feeds := []Feed{
		{Rkey: "nfl", DisplayName: "NFL", Paths: []string{"sports/american_football"}, MinProb: 0.5, Ranking: DefaultRanking},
		{Rkey: "for-you", DisplayName: "For you", Ranking: DefaultRanking, Personal: &cfg},
	}
	b := &refusesPersonal{t: t}
	fs := NewFeeds(&Config{Feeds: feeds}, b, slog.New(slog.NewTextHandler(io.Discard, nil)), time.Hour, 20*time.Millisecond, 10)
	ctx, cancel := context.WithCancel(context.Background())
	fs.Start(ctx)
	time.Sleep(100 * time.Millisecond) // several timer rebuilds
	cancel()
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(b.built) < 2 || b.built[0] != "nfl" {
		t.Errorf("the topic feed should have been rebuilt on the timer: %v", b.built)
	}
}

// --- over HTTP ------------------------------------------------------------------------

func TestPersonalFeedOverHTTP(t *testing.T) {
	viewer := newTestViewer(t, viewerDID)
	dir := testDirectory(viewer.doc())

	src := &fakeSource{likes: aiLover(), pool: twoTopicPool(30)}
	p, _, _ := newPersonalFor(t, src, nil)
	// /healthz measures how old the posts are against the real clock, so use it here.
	p.Now = nil
	p.refresh(context.Background(), p.feeds["for-you"])
	cfg := PersonalConfigDefaults()
	feeds := []Feed{
		{Rkey: "nfl", DisplayName: "NFL", Paths: []string{"sports/american_football"}, MinProb: 0.5, Ranking: DefaultRanking},
		{Rkey: "for-you", DisplayName: "For you", Ranking: DefaultRanking, Personal: &cfg},
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	fs := NewFeeds(&Config{Feeds: feeds}, fakeBuilder{posts(5)}, log, 48*time.Hour, time.Hour, 100)
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	fs.Start(ctx)
	s := NewServer(ServerConfig{Hostname: "feeds.example.com", ServiceDID: "did:web:feeds.example.com",
		OwnerDID: syntax.DID("did:plc:owner"), MaxAge: time.Hour}, fs, dir, log)
	s.Personal = p
	sink := &fakeSink{}
	s.Interactions = sink

	token := func(method syntax.NSID) string { return viewer.credential(t, "did:web:feeds.example.com", method) }
	feed := url.QueryEscape("at://did:plc:owner/app.bsky.feed.generator/for-you")
	ids := func(body map[string]any) []string {
		var out []string
		for _, it := range body["feed"].([]any) {
			out = append(out, it.(map[string]any)["post"].(string))
		}
		return out
	}

	// Without a credential there is no viewer: the welcome post.
	code, body := get(t, s, "/xrpc/app.bsky.feed.getFeedSkeleton?feed="+feed)
	if got := ids(body); code != 200 || len(got) != 1 || got[0] != welcomeURI {
		t.Fatalf("no credential: %d %v", code, body)
	}

	bearer := []string{"Authorization", "Bearer " + token(skeletonMethod)}
	code, body = get(t, s, "/xrpc/app.bsky.feed.getFeedSkeleton?limit=10&feed="+feed, bearer...)
	one := ids(body)
	if code != 200 || len(one) != 10 || body["cursor"] == nil || !strings.HasPrefix(body["reqId"].(string), "for-you-") {
		t.Fatalf("page 1: %d %v", code, body)
	}
	if fc := body["feed"].([]any)[0].(map[string]any)["feedContext"].(string); !strings.Contains(fc, `"topic"`) {
		t.Errorf("feedContext %q", fc)
	}
	code, body = get(t, s, "/xrpc/app.bsky.feed.getFeedSkeleton?limit=10&feed="+feed+"&cursor="+url.QueryEscape(body["cursor"].(string)), bearer...)
	two := ids(body)
	if code != 200 || len(two) != 10 {
		t.Fatalf("page 2: %d %v", code, body)
	}
	for _, u := range two {
		if slices.Contains(one, u) {
			t.Errorf("%s on both pages", u)
		}
	}
	if code, body := get(t, s, "/xrpc/app.bsky.feed.getFeedSkeleton?feed="+feed+"&cursor=nonsense", bearer...); code != 400 || body["error"] != "InvalidRequest" {
		t.Errorf("bad cursor: %d %v", code, body)
	}

	// Bluesky reports the first post of page 1 as seen: it is not shown again, though the
	// other nine (sent but not reported) are, once more.
	req := httptest.NewRequest(http.MethodPost, "/xrpc/app.bsky.feed.sendInteractions", strings.NewReader(
		`{"feed":"at://did:plc:owner/app.bsky.feed.generator/for-you","interactions":[{"item":"`+one[0]+`","event":"app.bsky.feed.defs#interactionSeen"}]}`))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+token(interactionsMethod))
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("sendInteractions: %d %s", rec.Code, rec.Body)
	}
	_, body = get(t, s, "/xrpc/app.bsky.feed.getFeedSkeleton?limit=100&feed="+feed, bearer...)
	again := ids(body)
	if slices.Contains(again, one[0]) {
		t.Error("a post reported as seen was shown again")
	}
	if !slices.Contains(again, one[1]) {
		t.Error("a post sent but never reported seen should get a second showing")
	}

	// The service stays healthy, lists the personal feed to Bluesky, and leaves it out of the
	// builder's directory (it has no settings to remix).
	if code, body := get(t, s, "/healthz"); code != 200 || body["ok"] != true {
		t.Errorf("healthz: %d %v", code, body)
	}
	_, desc := get(t, s, "/xrpc/app.bsky.feed.describeFeedGenerator")
	if len(desc["feeds"].([]any)) != 2 {
		t.Errorf("describe: %v", desc)
	}
	resp := httptest.NewRecorder()
	s.Handler().ServeHTTP(resp, httptest.NewRequest(http.MethodGet, "/api/feeds", nil))
	if strings.Contains(resp.Body.String(), "for-you") || !strings.Contains(resp.Body.String(), "nfl") {
		t.Errorf("/api/feeds: %s", resp.Body)
	}
}
