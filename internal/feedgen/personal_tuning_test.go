package feedgen

import (
	"context"
	"errors"
	"maps"
	"math"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

func shareMap(p Profile) map[string]float64 {
	out := map[string]float64{}
	for _, x := range p.Topics {
		out[x.Path] = x.Share
	}
	return out
}

func TestProfileFor(t *testing.T) {
	cfg := PersonalConfigDefaults() // 5 likes needed, 20 interests
	pool := map[string][]Post{}
	for _, topic := range []string{"g/one", "g/two", "g/three"} {
		pool[topic] = topicPosts(topic, minGenericPool)
	}
	mass := map[string]float64{"a/x": 8, "b/x": 4, "c/x": 2, "d/x": 2, unclearTopic: 100}
	w := func(kv ...any) Tuning {
		topics := map[string]float64{}
		for i := 0; i < len(kv); i += 2 {
			topics[kv[i].(string)] = kv[i+1].(float64)
		}
		return Tuning{Topics: topics}
	}

	for _, tc := range []struct {
		name  string
		cfg   func(*PersonalConfig)
		tune  Tuning
		mass  map[string]float64
		likes int
		pool  map[string][]Post
		state string
		want  map[string]float64 // shares
	}{
		{name: "enough likes, no tuning", mass: mass, likes: 20, pool: pool, state: StatePersonal,
			want: map[string]float64{"a/x": .5, "b/x": .25, "c/x": .125, "d/x": .125}},
		{name: "a muted interest leaves the feed", tune: w("a/x", 0.0), mass: mass, likes: 20, pool: pool, state: StatePersonal,
			want: map[string]float64{"b/x": .5, "c/x": .25, "d/x": .25}},
		{name: "a turned-up interest takes a bigger share", tune: w("b/x", 2.0), mass: mass, likes: 20, pool: pool, state: StatePersonal,
			want: map[string]float64{"a/x": .4, "b/x": .4, "c/x": .1, "d/x": .1}},
		{name: "an added topic starts at the median interest", tune: w("e/x", 1.0), mass: mass, likes: 20, pool: pool, state: StatePersonal,
			// the median of 8, 4, 2, 2 is 3
			want: map[string]float64{"a/x": 8.0 / 19, "b/x": 4.0 / 19, "c/x": 2.0 / 19, "d/x": 2.0 / 19, "e/x": 3.0 / 19}},
		{name: "muting makes room for the next interest", cfg: func(c *PersonalConfig) { c.Topics = 2 },
			tune: w("a/x", 0.0), mass: map[string]float64{"a/x": 10, "b/x": 5, "c/x": 3}, likes: 20, pool: pool, state: StatePersonal,
			want: map[string]float64{"b/x": .625, "c/x": .375}},
		{name: "too few likes: a mix of every topic", mass: map[string]float64{"a/x": 2}, likes: 2, pool: pool, state: StateGeneric,
			want: map[string]float64{"g/one": 1.0 / 3, "g/two": 1.0 / 3, "g/three": 1.0 / 3}},
		{name: "too few likes: the mix leaves out what is muted", tune: w("g/one", 0.0), mass: map[string]float64{"a/x": 2}, likes: 2, pool: pool, state: StateGeneric,
			want: map[string]float64{"g/two": .5, "g/three": .5}},
		{name: "too few likes: topics they turned on are their interests", tune: w("e/x", 2.0, "f/x", 1.0, "g/one", 0.0),
			mass: map[string]float64{"a/x": 2}, likes: 2, pool: pool, state: StatePersonal,
			want: map[string]float64{"e/x": 2.0 / 3, "f/x": 1.0 / 3}},
		{name: "no likes at all can still turn topics on", tune: w("e/x", 3.0), likes: 0, pool: pool, state: StatePersonal,
			want: map[string]float64{"e/x": 1}},
		{name: "everything muted gives the mix, still leaving out what is muted", tune: w("a/x", 0.0, "b/x", 0.0, "c/x", 0.0, "d/x", 0.0, "g/two", 0.0),
			mass: mass, likes: 20, pool: pool, state: StateGeneric,
			want: map[string]float64{"g/one": .5, "g/three": .5}},
		{name: "everything muted and no mix to give: nothing", tune: w("a/x", 0.0, "b/x", 0.0, "c/x", 0.0, "d/x", 0.0),
			mass: mass, likes: 20, state: StateGeneric, want: map[string]float64{}},
		{name: "likes of posts the model couldn't place say nothing", mass: map[string]float64{unclearTopic: 50}, likes: 10, pool: pool, state: StateGeneric,
			want: map[string]float64{"g/one": 1.0 / 3, "g/two": 1.0 / 3, "g/three": 1.0 / 3}},
		{name: "nothing to go on and no mix", state: StateGeneric, want: map[string]float64{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c := cfg
			if tc.cfg != nil {
				tc.cfg(&c)
			}
			before := maps.Clone(tc.mass)
			prof, state := ProfileFor(c, tc.tune, tc.mass, tc.likes, tc.pool, now)
			if state != tc.state {
				t.Errorf("state %q, want %q", state, tc.state)
			}
			got := shareMap(prof)
			if len(got) != len(tc.want) {
				t.Fatalf("interests %v, want %v", got, tc.want)
			}
			for path, share := range tc.want {
				if g, ok := got[path]; !ok || !near(g, share) {
					t.Errorf("%s: share %v, want %v (all: %v)", path, got[path], share, got)
				}
			}
			if !maps.Equal(tc.mass, before) {
				t.Error("the viewer's likes were changed")
			}
		})
	}
}

// fakeTunings is a TuningStore in memory.
type fakeTunings struct {
	mu      sync.Mutex
	saved   map[string]Tuning
	readErr error
	saveErr error
	reads   int
	saves   int
}

func newTunings(did string, t *Tuning) *fakeTunings {
	f := &fakeTunings{saved: map[string]Tuning{}}
	if t != nil {
		f.saved[did] = *t
	}
	return f
}

func (f *fakeTunings) ViewerTuning(_ context.Context, did string) (Tuning, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.reads++
	if f.readErr != nil {
		return Tuning{}, f.readErr
	}
	t := f.saved[did]
	t.Topics = maps.Clone(t.Topics)
	return t, nil
}

func (f *fakeTunings) SaveTuning(_ context.Context, did string, t Tuning) error {
	f.mu.Lock()
	defer f.mu.Unlock()
	if f.saveErr != nil {
		return f.saveErr
	}
	f.saves++
	t.Topics = maps.Clone(t.Topics)
	f.saved[did] = t
	return nil
}

func (f *fakeTunings) get(did string) Tuning {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.saved[did]
}

func (f *fakeTunings) counts() (reads, saves int) {
	f.mu.Lock()
	defer f.mu.Unlock()
	return f.reads, f.saves
}

func (f *fakeTunings) failReads(err error) {
	f.mu.Lock()
	f.readErr = err
	f.mu.Unlock()
}

func (f *fakeTunings) failSaves(err error) {
	f.mu.Lock()
	f.saveErr = err
	f.mu.Unlock()
}

func (f *fakeSource) failLikes(err error) {
	f.mu.Lock()
	f.likesErr = err
	f.mu.Unlock()
}

// tunedPersonal is newPersonalFor with a tuning store.
func tunedPersonal(t *testing.T, src *fakeSource, tun *fakeTunings, tweak func(*PersonalConfig)) (*Personal, *testClock, *servedSink) {
	t.Helper()
	p, clock, sink := newPersonalFor(t, src, tweak)
	p.Tunings = tun // before anyone is read
	return p, clock, sink
}

func mixOf(pool map[string][]Post, pg PersonalPage) map[string]int {
	return topicCounts(postsOf(pool, itemURIs(pg)))
}

func eventually(t *testing.T, what string, ok func() bool) {
	t.Helper()
	for range 600 {
		if ok() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

const (
	ai    = "technology/ai"
	cats  = "animals_nature/cats"
	bball = "sports/baseball"
)

// bigPool is how many posts each topic has in tests that ask for pages over and over until
// something changes. Each page uses up posts (a post is shown at most max_serves times), so
// with a small pool the viewer would run out of one topic first, and a feed that has run
// out looks just like a feed that was tuned.
const bigPool = 1000

func threeTopicPool(n int) map[string][]Post {
	pool := twoTopicPool(n)
	pool[bball] = topicPosts(bball, n)
	return pool
}

func TestPersonalAppliesTheViewersSavedTuning(t *testing.T) {
	// The viewer liked AI three times as much as cats: 45 of 60 slots, then 15.
	for _, tc := range []struct {
		name string
		tune Tuning
		want map[string]int // posts of each topic in the first 60
	}{
		{"no tuning", Tuning{}, map[string]int{ai: 45, cats: 15}},
		{"a muted interest is gone", Tuning{Topics: map[string]float64{ai: 0}}, map[string]int{cats: 60}},
		{"the other muted", Tuning{Topics: map[string]float64{cats: 0}}, map[string]int{ai: 60}},
		{"turning cats up evens it out", Tuning{Topics: map[string]float64{cats: 3}}, map[string]int{ai: 30, cats: 30}},
		{"a topic added next to the interests", Tuning{Topics: map[string]float64{bball: 1}}, map[string]int{ai: 30, cats: 10, bball: 20}},
		{"settings that aren't about topics change no topic", Tuning{Freshness: FreshnessFresh, HidePromo: true}, map[string]int{ai: 45, cats: 15}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			pool := threeTopicPool(100)
			src := &fakeSource{likes: aiLover(), pool: pool}
			tun := newTunings(viewerDID, &tc.tune)
			p, _, _ := tunedPersonal(t, src, tun, func(c *PersonalConfig) { *c.AuthorGap = 0 })
			pg := page(t, p, viewerDID, "", 60)
			if pg.State != StatePersonal {
				t.Errorf("state %q", pg.State)
			}
			got := mixOf(pool, pg)
			if len(got) != len(tc.want) {
				t.Fatalf("%v, want %v", got, tc.want)
			}
			for topic, n := range tc.want {
				if got[topic] != n {
					t.Errorf("%v, want %v", got, tc.want)
					break
				}
			}
			if reads, _ := tun.counts(); reads != 1 {
				t.Errorf("tuning read %d times, want once when the viewer was read", reads)
			}
		})
	}
}

func TestPersonalWithoutATuningStoreIsUntuned(t *testing.T) {
	pool := twoTopicPool(100)
	p, _, _ := newPersonalFor(t, &fakeSource{likes: aiLover(), pool: pool}, func(c *PersonalConfig) { *c.AuthorGap = 0 })
	if got := mixOf(pool, page(t, p, viewerDID, "", 40)); got[ai] != 30 || got[cats] != 10 {
		t.Errorf("%v", got)
	}
}

func TestPersonalTuningIsPerViewer(t *testing.T) {
	pool := twoTopicPool(100)
	tun := newTunings("did:plc:picky", &Tuning{Topics: map[string]float64{ai: 0}})
	p, _, _ := tunedPersonal(t, &fakeSource{likes: aiLover(), pool: pool}, tun, func(c *PersonalConfig) { *c.AuthorGap = 0 })
	if got := mixOf(pool, page(t, p, "did:plc:picky", "", 40)); got[ai] != 0 || got[cats] != 40 {
		t.Errorf("the viewer who muted AI: %v", got)
	}
	if got := mixOf(pool, page(t, p, "did:plc:other", "", 40)); got[ai] != 30 || got[cats] != 10 {
		t.Errorf("another viewer is untouched: %v", got)
	}
}

// oldAndFresh is an old post with a lot of likes, and one just posted with none: popular
// ranks the old one first, fresh the new one.
func oldAndFresh() (old, fresh Post) {
	old = Post{URI: "at://did:plc:old/app.bsky.feed.post/1", DID: "did:plc:old", IndexedAt: now.Add(-20 * time.Hour), Likes: 100, TopPath: ai, TopPathP: 0.9}
	fresh = Post{URI: "at://did:plc:new/app.bsky.feed.post/1", DID: "did:plc:new", IndexedAt: now.Add(-time.Minute), TopPath: ai, TopPathP: 0.9}
	return old, fresh
}

func TestPersonalTuningFreshness(t *testing.T) {
	old, fresh := oldAndFresh()
	pool := map[string][]Post{ai: {old, fresh}}
	first := func(f string) string {
		src := &fakeSource{likes: aiLover(), pool: pool}
		p, _, _ := tunedPersonal(t, src, newTunings(viewerDID, &Tuning{Freshness: f}), nil)
		pg := page(t, p, viewerDID, "", 10)
		if len(pg.Items) != 2 {
			t.Fatalf("%s: %d posts", f, len(pg.Items))
		}
		return pg.Items[0].URI
	}
	if got := first(FreshnessPopular); got != old.URI {
		t.Errorf("popular puts %s first, want the well-liked old post", got)
	}
	if got := first(FreshnessFresh); got != fresh.URI {
		t.Errorf("fresh puts %s first, want the new post", got)
	}
	if got := first(""); got != old.URI {
		t.Errorf("the feed's own ranking puts %s first", got)
	}
}

func TestPersonalTuningHidesPosts(t *testing.T) {
	posts := topicPosts(ai, 30)
	for i := range posts {
		switch {
		case i%3 == 0:
			posts[i].Signals = map[string]float32{"spam": 0.5, "substance": 0.9}
		case i%3 == 1:
			posts[i].Tone = map[string]float32{"outraged": 0.9}
			posts[i].Signals = map[string]float32{"substance": 0.9}
		default:
			posts[i].Signals = map[string]float32{"substance": 0.1}
		}
	}
	pool := map[string][]Post{ai: posts}
	hidden := func(tune Tuning, kind int) (shown, wrong int) {
		src := &fakeSource{likes: aiLover(), pool: pool}
		p, _, _ := tunedPersonal(t, src, newTunings(viewerDID, &tune), func(c *PersonalConfig) { *c.AuthorGap = 0 })
		idx := map[string]int{}
		for i, post := range posts {
			idx[post.URI] = i
		}
		pg := page(t, p, viewerDID, "", 100)
		for _, it := range pg.Items {
			shown++
			if idx[it.URI]%3 == kind {
				wrong++
			}
		}
		return shown, wrong
	}
	if shown, spam := hidden(Tuning{HidePromo: true}, 0); shown != 20 || spam != 0 {
		t.Errorf("hide promotional: %d shown, %d of them spam, want 20 and 0", shown, spam)
	}
	if shown, angry := hidden(Tuning{Tone: Rules{Max: map[string]float32{"outraged": 0.5}}}, 1); shown != 20 || angry != 0 {
		t.Errorf("hide angry: %d shown, %d of them angry, want 20 and 0", shown, angry)
	}
	if shown, thin := hidden(Tuning{Signals: Rules{Min: map[string]float32{"substance": 0.5}}}, 2); shown != 20 || thin != 0 {
		t.Errorf("minimum substance: %d shown, %d of them thin, want 20 and 0", shown, thin)
	}
	if shown, _ := hidden(Tuning{}, 0); shown != 30 {
		t.Errorf("no tuning: %d shown, want all 30", shown)
	}
}

func TestPersonalTuningAuthorGap(t *testing.T) {
	// The six best posts are by one author; six more are by six others.
	posts := topicPosts(ai, 12)
	for i := range 6 {
		posts[i].DID = "did:plc:prolific"
	}
	pool := map[string][]Post{ai: posts}
	run := func(tune Tuning) []Post {
		p, _, _ := tunedPersonal(t, &fakeSource{likes: aiLover(), pool: pool}, newTunings(viewerDID, &tune), nil)
		pg := page(t, p, viewerDID, "", 6)
		return postsOf(pool, itemURIs(pg))
	}
	byProlific := func(ps []Post) (n int) {
		for _, p := range ps {
			if p.DID == "did:plc:prolific" {
				n++
			}
		}
		return n
	}
	if n := byProlific(run(Tuning{})); n >= 6 {
		t.Errorf("with the feed's gap of 10, the first 6 are all one author's: %d", n)
	}
	if n := byProlific(run(Tuning{AuthorGap: ptr(0)})); n != 6 {
		t.Errorf("with no gap the six best posts come first: %d of them by the one author, want 6", n)
	}
	if n := byProlific(run(Tuning{AuthorGap: ptr(2)})); n >= 5 {
		t.Errorf("with a gap of 2, %d of the first 6 are one author's", n)
	}
}

func TestPersonalViewerWhoHasLikedLittleCanTurnTopicsOn(t *testing.T) {
	few := LikeData{Mass: map[string]float64{ai: 2}, Posts: 2, Liked: map[string]struct{}{}}
	pool := threeTopicPool(30)
	mix := func(tune *Tuning) (PersonalPage, map[string]int) {
		p, _, _ := tunedPersonal(t, &fakeSource{likes: few, pool: pool}, newTunings(viewerDID, tune), func(c *PersonalConfig) { *c.AuthorGap = 0 })
		pg := page(t, p, viewerDID, "", 30)
		return pg, mixOf(pool, pg)
	}
	pg, got := mix(nil)
	if pg.State != StateGeneric || len(got) != 3 {
		t.Errorf("untuned: %s %v, want a mix of every topic", pg.State, got)
	}
	pg, got = mix(&Tuning{Topics: map[string]float64{bball: 1}})
	if pg.State != StatePersonal || len(got) != 1 || got[bball] != 30 {
		t.Errorf("with baseball turned on: %s %v, want baseball only", pg.State, got)
	}
	pg, got = mix(&Tuning{Topics: map[string]float64{ai: 0}})
	if pg.State != StateGeneric || got[ai] != 0 || got[cats] == 0 || got[bball] == 0 {
		t.Errorf("with AI muted: %s %v, want the mix without AI", pg.State, got)
	}
}

func TestSetTuningAppliesAtOnceAndSaves(t *testing.T) {
	pool := twoTopicPool(100)
	tun := newTunings(viewerDID, nil)
	p, _, _ := tunedPersonal(t, &fakeSource{likes: aiLover(), pool: pool}, tun, func(c *PersonalConfig) { *c.AuthorGap = 0 })
	if got := mixOf(pool, page(t, p, viewerDID, "", 40)); got[ai] != 30 {
		t.Fatalf("before: %v", got)
	}

	mute := Tuning{Topics: map[string]float64{ai: 0}}
	if err := p.SetTuning(context.Background(), viewerDID, mute); err != nil {
		t.Fatal(err)
	}
	if got := tun.get(viewerDID); got.Topics[ai] != 0 || len(got.Topics) != 1 {
		t.Errorf("saved %+v", got)
	}
	if got := mixOf(pool, page(t, p, viewerDID, "", 40)); got[ai] != 0 || got[cats] != 40 {
		t.Errorf("right after: %v, want cats only", got)
	}

	if err := p.SetTuning(context.Background(), viewerDID, Tuning{}); err != nil {
		t.Fatal(err)
	}
	if got := mixOf(pool, page(t, p, viewerDID, "", 20)); got[ai] == 0 {
		t.Errorf("tuning cleared: %v, want AI back", got)
	}
}

func TestSetTuningKeepsWhatTheViewerHasSeen(t *testing.T) {
	pool := twoTopicPool(30)
	p, _, _ := tunedPersonal(t, &fakeSource{likes: aiLover(), pool: pool}, newTunings(viewerDID, nil),
		func(c *PersonalConfig) { c.MaxServes = 1; *c.AuthorGap = 0 })
	first := page(t, p, viewerDID, "", 20)
	if len(first.Items) != 20 || first.Cursor == "" {
		t.Fatalf("first page: %d posts, cursor %q", len(first.Items), first.Cursor)
	}
	if err := p.SetTuning(context.Background(), viewerDID, Tuning{Topics: map[string]float64{cats: 5}}); err != nil {
		t.Fatal(err)
	}
	// The cursor is from the feed that was replaced: the next page starts the new one over,
	// without anything already sent.
	next := page(t, p, viewerDID, first.Cursor, 20)
	if len(next.Items) != 20 {
		t.Fatalf("%d posts after the change", len(next.Items))
	}
	sent := map[string]bool{}
	for _, u := range itemURIs(first) {
		sent[u] = true
	}
	for _, u := range itemURIs(next) {
		if sent[u] {
			t.Errorf("%s was sent again", u)
		}
	}
	if got := mixOf(pool, next); got[cats] <= got[ai] {
		t.Errorf("after turning cats up: %v", got)
	}
}

func TestSetTuningRefusesWhatIsInvalidOrCantBeSaved(t *testing.T) {
	pool := twoTopicPool(100)
	tun := newTunings(viewerDID, nil)
	p, _, _ := tunedPersonal(t, &fakeSource{likes: aiLover(), pool: pool}, tun, func(c *PersonalConfig) { *c.AuthorGap = 0 })
	ctx := context.Background()
	page(t, p, viewerDID, "", 10)

	for name, bad := range map[string]Tuning{
		"a weight above the maximum": {Topics: map[string]float64{ai: 9}},
		"unknown freshness":          {Freshness: "frozen"},
		"a half-life of zero days":   {HalfLifeDays: 0.1},
		"a NaN":                      {HalfLifeDays: math.NaN()},
		"a signal that isn't one":    {Signals: Rules{Min: map[string]float32{"vibes": 0.5}}},
		"a gravity past the limit":   {Ranking: &RankingTuning{Gravity: ptr(11.0)}},
		"a lookback past the limit":  {LookbackDays: 99},
	} {
		if err := p.SetTuning(ctx, viewerDID, bad); err == nil {
			t.Errorf("%s was accepted", name)
		}
	}
	if _, saves := tun.counts(); saves != 0 {
		t.Errorf("%d invalid tunings were saved", saves)
	}

	tun.failSaves(errors.New("clickhouse is down"))
	if err := p.SetTuning(ctx, viewerDID, Tuning{Topics: map[string]float64{ai: 0}}); err == nil {
		t.Error("a failed save was reported as done")
	}
	if got := mixOf(pool, page(t, p, viewerDID, "", 40)); got[ai] != 30 {
		t.Errorf("a failed save changed the feed: %v", got)
	}

	// With nowhere to save, it says so rather than keeping it in memory only.
	p.Tunings = nil
	if err := p.SetTuning(ctx, viewerDID, Tuning{}); !errors.Is(err, errNoTuningStore) {
		t.Errorf("err %v", err)
	}
}

func TestSetTuningForAViewerNotInMemoryIsSavedForWhenTheyArrive(t *testing.T) {
	pool := twoTopicPool(100)
	tun := newTunings(viewerDID, nil)
	p, _, _ := tunedPersonal(t, &fakeSource{likes: aiLover(), pool: pool}, tun, func(c *PersonalConfig) { *c.AuthorGap = 0 })
	if err := p.SetTuning(context.Background(), viewerDID, Tuning{Topics: map[string]float64{ai: 0}}); err != nil {
		t.Fatal(err)
	}
	if got := mixOf(pool, page(t, p, viewerDID, "", 40)); got[ai] != 0 {
		t.Errorf("%v", got)
	}
}

// halfLifeLikes: a viewer who liked AI a lot a while ago and cats a lot lately: how much each
// counts depends on how fast old likes fade.
func halfLifeLikes(hl time.Duration) LikeData {
	d := LikeData{Posts: 40, Liked: map[string]struct{}{}}
	if hl <= 24*time.Hour {
		d.Mass = map[string]float64{ai: 5, cats: 35}
	} else {
		d.Mass = map[string]float64{ai: 35, cats: 5}
	}
	return d
}

func TestSetTuningReadsLikesAgainWhenTheMemoryChanges(t *testing.T) {
	pool := twoTopicPool(100)
	src := &fakeSource{likesFor: halfLifeLikes, pool: pool}
	p, _, _ := tunedPersonal(t, src, newTunings(viewerDID, nil), func(c *PersonalConfig) { *c.AuthorGap = 0; c.MaxServes = 20 })
	day, week := 24*time.Hour, 7*24*time.Hour
	lean := func() (aiN, catsN int) {
		got := mixOf(pool, page(t, p, viewerDID, "", 40))
		return got[ai], got[cats]
	}
	if a, c := lean(); a <= c {
		t.Fatalf("a week's memory: %d AI, %d cats", a, c)
	}

	if err := p.SetTuning(context.Background(), viewerDID, Tuning{HalfLifeDays: 1}); err != nil {
		t.Fatal(err)
	}
	if a, c := lean(); a >= c {
		t.Errorf("a day's memory, right after saving: %d AI, %d cats, want mostly cats", a, c)
	}
	// A change that doesn't touch the memory doesn't read likes again.
	if err := p.SetTuning(context.Background(), viewerDID, Tuning{HalfLifeDays: 1, Freshness: FreshnessFresh}); err != nil {
		t.Fatal(err)
	}
	if err := p.SetTuning(context.Background(), viewerDID, Tuning{}); err != nil {
		t.Fatal(err)
	}
	if a, c := lean(); a <= c {
		t.Errorf("back to a week: %d AI, %d cats", a, c)
	}
	got := src.halfLives()
	want := []time.Duration{week, day, week}
	if len(got) != len(want) {
		t.Fatalf("likes read with half-lives %v, want %v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("likes read with half-lives %v, want %v", got, want)
		}
	}
}

func TestSetTuningWhenLikesCantBeReadAgainTriesAgainLater(t *testing.T) {
	pool := twoTopicPool(bigPool) // polling uses up posts: there must be more than polling can use
	src := &fakeSource{likesFor: halfLifeLikes, pool: pool}
	p, clock, _ := tunedPersonal(t, src, newTunings(viewerDID, nil), func(c *PersonalConfig) { *c.AuthorGap = 0; c.MaxServes = 20 })
	lean := func() (int, int) {
		got := mixOf(pool, page(t, p, viewerDID, "", 10))
		return got[ai], got[cats]
	}
	lean()
	src.failLikes(errors.New("clickhouse is down"))
	if err := p.SetTuning(context.Background(), viewerDID, Tuning{HalfLifeDays: 1, Topics: map[string]float64{bball: 0}}); err != nil {
		t.Fatalf("the settings are saved even though the likes couldn't be read again: %v", err)
	}
	if a, c := lean(); a <= c {
		t.Errorf("until they can be read, the old weighting stays: %d AI, %d cats", a, c)
	}
	src.failLikes(nil)
	// A failed read is retried after a minute, by the first request after that.
	clock.Advance(time.Minute + time.Second)
	eventually(t, "the likes to be read again with the new memory", func() bool {
		a, c := lean()
		return a < c
	})
}

func TestTuningSavedWhileTheViewerIsBeingReadWins(t *testing.T) {
	pool := twoTopicPool(100)
	src := &fakeSource{likes: aiLover(), pool: pool, delay: 300 * time.Millisecond}
	tun := newTunings(viewerDID, nil)
	p, _, _ := tunedPersonal(t, src, tun, func(c *PersonalConfig) { *c.AuthorGap = 0 })
	p.Budget = time.Millisecond
	if pg := page(t, p, viewerDID, "", 40); pg.State != "welcome" {
		t.Fatalf("%+v", pg)
	}
	// The read of their tuning found nothing; then they save one while their likes are read.
	eventually(t, "the viewer's tuning to be read", func() bool { r, _ := tun.counts(); return r == 1 })
	if err := p.SetTuning(context.Background(), viewerDID, Tuning{Topics: map[string]float64{ai: 0}}); err != nil {
		t.Fatal(err)
	}
	pg := waitPersonal(t, p, viewerDID, 40)
	if got := mixOf(pool, pg); got[ai] != 0 || got[cats] != 40 {
		t.Errorf("the tuning saved during the read was overwritten by the one read before it: %v", got)
	}
}

func TestMemorySavedWhileTheViewerIsBeingReadIsAppliedByTheNextRead(t *testing.T) {
	pool := twoTopicPool(bigPool)
	src := &fakeSource{likesFor: halfLifeLikes, pool: pool, delay: 200 * time.Millisecond}
	tun := newTunings(viewerDID, nil)
	p, _, _ := tunedPersonal(t, src, tun, func(c *PersonalConfig) { *c.AuthorGap = 0; c.MaxServes = 20 })
	p.Budget = time.Millisecond
	page(t, p, viewerDID, "", 10)
	eventually(t, "the viewer's tuning to be read", func() bool { r, _ := tun.counts(); return r == 1 })
	if err := p.SetTuning(context.Background(), viewerDID, Tuning{HalfLifeDays: 1}); err != nil {
		t.Fatal(err)
	}
	// The read in progress used a week; the next one must use a day.
	eventually(t, "likes weighted by a day's memory", func() bool {
		pg := page(t, p, viewerDID, "", 10)
		got := mixOf(pool, pg)
		return pg.State != "welcome" && got[cats] > got[ai]
	})
}

func viewerOf(p *Personal, did string) *viewerState {
	p.mu.Lock()
	defer p.mu.Unlock()
	return p.viewers[viewerKey{"for-you", did}]
}

func TestAReadStartedBeforeTheMemoryChangedDoesNotOverwriteIt(t *testing.T) {
	pool := twoTopicPool(bigPool)
	week := 7 * 24 * time.Hour
	release := make(chan struct{})
	var holding atomic.Bool
	src := &fakeSource{likesFor: halfLifeLikes, pool: pool, hold: func(hl time.Duration) chan struct{} {
		if holding.Load() && hl == week {
			return release
		}
		return nil
	}}
	p, clock, _ := tunedPersonal(t, src, newTunings(viewerDID, nil), func(c *PersonalConfig) { *c.AuthorGap = 0; c.MaxServes = 20 })
	page(t, p, viewerDID, "", 10)

	// Their likes are due to be read again, with a week's memory; that read is slow...
	holding.Store(true)
	clock.Advance(profileTTL + time.Second)
	page(t, p, viewerDID, "", 1)
	eventually(t, "the second read of their likes to start", func() bool { return len(src.halfLives()) == 2 })
	// ...and they change the memory to a day, which is read at once.
	if err := p.SetTuning(context.Background(), viewerDID, Tuning{HalfLifeDays: 1}); err != nil {
		t.Fatal(err)
	}
	// When the slow read finishes, it is a week's worth and must not replace the day's.
	close(release)
	vs := viewerOf(p, viewerDID)
	eventually(t, "the slow read to finish", func() bool {
		vs.mu.Lock()
		defer vs.mu.Unlock()
		return !vs.refreshing
	})
	if got := mixOf(pool, page(t, p, viewerDID, "", 10)); got[cats] <= got[ai] {
		t.Errorf("%v: the likes weighted by the old memory replaced the new", got)
	}
}

func TestViewerSavedTuningWithItsOwnMemoryReadsLikesThatWay(t *testing.T) {
	pool := twoTopicPool(100)
	src := &fakeSource{likesFor: halfLifeLikes, pool: pool}
	p, _, _ := tunedPersonal(t, src, newTunings(viewerDID, &Tuning{HalfLifeDays: 1}), func(c *PersonalConfig) { *c.AuthorGap = 0 })
	got := mixOf(pool, page(t, p, viewerDID, "", 40))
	if got[cats] <= got[ai] {
		t.Errorf("%v: with a day's memory, mostly cats", got)
	}
	if hls := src.halfLives(); len(hls) != 2 || hls[1] != 24*time.Hour {
		t.Errorf("likes read with %v, want the feed's half-life, then a day", hls)
	}
}

func TestPersonalFeedWorksUntunedWhenTheTuningCantBeRead(t *testing.T) {
	pool := twoTopicPool(bigPool)
	tun := newTunings(viewerDID, &Tuning{Topics: map[string]float64{ai: 0}})
	tun.failReads(errors.New("clickhouse is down"))
	p, clock, _ := tunedPersonal(t, &fakeSource{likes: aiLover(), pool: pool}, tun, func(c *PersonalConfig) { *c.AuthorGap = 0; c.MaxServes = 20 })

	pg := page(t, p, viewerDID, "", 10)
	if pg.State != StatePersonal || len(pg.Items) != 10 {
		t.Fatalf("a failed tuning read must not fail the feed: %+v", pg)
	}
	if got := mixOf(pool, pg); got[ai] == 0 {
		t.Errorf("untuned until it can be read: %v", got)
	}

	// Once it can be read, the next refresh picks it up.
	tun.failReads(nil)
	page(t, p, viewerDID, "", 1) // not due yet
	clock.Advance(retryAfter + time.Second)
	eventually(t, "the saved tuning to be applied", func() bool {
		got := mixOf(pool, page(t, p, viewerDID, "", 10))
		return got[cats] == 10
	})
	if reads, _ := tun.counts(); reads > 3 {
		t.Errorf("the tuning was read %d times", reads)
	}
}

func TestPersonalIgnoresASavedTuningThatIsNotValid(t *testing.T) {
	pool := twoTopicPool(100)
	tun := newTunings(viewerDID, &Tuning{Freshness: "frozen", HalfLifeDays: -3, Topics: map[string]float64{ai: -1}})
	p, _, _ := tunedPersonal(t, &fakeSource{likes: aiLover(), pool: pool}, tun, func(c *PersonalConfig) { *c.AuthorGap = 0 })
	if got := mixOf(pool, page(t, p, viewerDID, "", 40)); got[ai] != 30 || got[cats] != 10 {
		t.Errorf("an invalid saved tuning should count as none: %v", got)
	}
	if reads, _ := tun.counts(); reads != 1 {
		t.Errorf("read %d times: an invalid tuning is not retried", reads)
	}
}

func TestPreviewShowsTheDraftWithoutSideEffects(t *testing.T) {
	pool := twoTopicPool(60)
	likes := aiLover()
	liked := pool[ai][1].URI
	likes.Liked[liked] = struct{}{}
	pool[ai][2].DID = viewerDID
	own := pool[ai][2].URI
	seen := pool[ai][0].URI
	src := &fakeSource{likes: likes, pool: pool, seen: SeenData{Confirmed: map[string]struct{}{seen: {}}}}
	tun := newTunings(viewerDID, nil)
	p, _, sink := tunedPersonal(t, src, tun, func(c *PersonalConfig) { c.MaxServes = 1; *c.AuthorGap = 0 })
	ctx := context.Background()

	posts, state, err := previewList(p, ctx, "for-you", viewerDID, Tuning{}, 30)
	if err != nil || state != StatePersonal || len(posts) != 30 {
		t.Fatalf("%d posts, state %q, err %v", len(posts), state, err)
	}
	var sawSeen bool
	for _, post := range posts {
		switch post.URI {
		case seen:
			sawSeen = true
		case liked, own:
			t.Errorf("%s is the viewer's own or liked and shouldn't be previewed", post.URI)
		}
	}
	if !sawSeen {
		t.Error("a preview shows what the settings pick, including posts the viewer has already seen")
	}

	// A draft: nothing of AI, no promotional posts.
	posts, _, err = previewList(p, ctx, "for-you", viewerDID, Tuning{Topics: map[string]float64{ai: 0}}, 30)
	if err != nil || len(posts) != 30 {
		t.Fatalf("%d posts, err %v", len(posts), err)
	}
	for _, post := range posts {
		if post.TopPath == ai {
			t.Errorf("the draft mutes AI but %s was previewed", post.URI)
		}
	}

	// Nothing was sent, saved, or used up.
	if len(sink.rows) != 0 {
		t.Errorf("a preview stored %d served posts", len(sink.rows))
	}
	if _, saves := tun.counts(); saves != 0 {
		t.Error("a draft was saved")
	}
	pg := page(t, p, viewerDID, "", 200)
	if len(pg.Items) != 117 { // 120 posts, less the seen, liked, and own ones
		t.Errorf("after previews the feed has %d posts, want 117: previews must not use posts up", len(pg.Items))
	}
	if got := mixOf(pool, pg); got[ai] == 0 {
		t.Errorf("the saved (empty) tuning is what feeds use, not the draft: %v", got)
	}
}

func TestPreviewAppliesTheDraftsSettings(t *testing.T) {
	posts := topicPosts(ai, 20)
	for i := range 5 {
		posts[i].Signals = map[string]float32{"ad": 0.9}
	}
	pool := map[string][]Post{ai: posts}
	p, _, _ := tunedPersonal(t, &fakeSource{likes: aiLover(), pool: pool}, newTunings(viewerDID, nil), func(c *PersonalConfig) { *c.AuthorGap = 0 })
	got, _, err := previewList(p, context.Background(), "for-you", viewerDID, Tuning{HidePromo: true}, 50)
	if err != nil || len(got) != 15 {
		t.Fatalf("%d posts, err %v, want the 15 without ads", len(got), err)
	}
	got, _, _ = previewList(p, context.Background(), "for-you", viewerDID, Tuning{}, 50)
	if len(got) != 20 {
		t.Errorf("%d posts without the filter", len(got))
	}
	if got, _, _ = previewList(p, context.Background(), "for-you", viewerDID, Tuning{}, 0); len(got) != 1 {
		t.Errorf("a limit of 0 gives %d posts, want 1", len(got))
	}
	if got, _, _ = previewList(p, context.Background(), "for-you", viewerDID, Tuning{}, 100000); len(got) != 20 {
		t.Errorf("a huge limit gives %d posts", len(got))
	}
}

func TestPreviewWithAnotherMemoryReadsLikesThatWay(t *testing.T) {
	pool := twoTopicPool(100)
	src := &fakeSource{likesFor: halfLifeLikes, pool: pool}
	p, _, _ := tunedPersonal(t, src, newTunings(viewerDID, nil), func(c *PersonalConfig) { *c.AuthorGap = 0 })
	ctx := context.Background()
	mix := func(tune Tuning) map[string]int {
		posts, _, err := previewList(p, ctx, "for-you", viewerDID, tune, 40)
		if err != nil {
			t.Fatal(err)
		}
		return topicCounts(posts)
	}
	if got := mix(Tuning{}); got[ai] <= got[cats] {
		t.Errorf("the week's memory: %v", got)
	}
	if got := mix(Tuning{HalfLifeDays: 1}); got[cats] <= got[ai] {
		t.Errorf("a day's memory: %v", got)
	}
	// What feeds use is still the saved settings: the previewed memory wasn't kept.
	if got := mixOf(pool, page(t, p, viewerDID, "", 40)); got[ai] <= got[cats] {
		t.Errorf("the feed: %v", got)
	}
}

func TestPreviewUsesTheDraftsFreshness(t *testing.T) {
	old, fresh := oldAndFresh()
	pool := map[string][]Post{ai: {old, fresh}}
	p, _, _ := tunedPersonal(t, &fakeSource{likes: aiLover(), pool: pool}, newTunings(viewerDID, nil), nil)
	first := func(freshness string) string {
		posts, _, err := previewList(p, context.Background(), "for-you", viewerDID, Tuning{Freshness: freshness}, 10)
		if err != nil || len(posts) != 2 {
			t.Fatalf("%s: %d posts, err %v", freshness, len(posts), err)
		}
		return posts[0].URI
	}
	if got := first(FreshnessPopular); got != old.URI {
		t.Errorf("popular puts %s first", got)
	}
	if got := first(FreshnessFresh); got != fresh.URI {
		t.Errorf("fresh puts %s first", got)
	}
}

func TestPreviewBuildsAViewerWhoHasNotOpenedTheFeed(t *testing.T) {
	pool := twoTopicPool(30)
	src := &fakeSource{likes: aiLover(), pool: pool, delay: 50 * time.Millisecond}
	p, _, _ := tunedPersonal(t, src, newTunings(viewerDID, nil), nil)
	posts, _, err := previewList(p, context.Background(), "for-you", viewerDID, Tuning{}, 10)
	if err != nil || len(posts) != 10 {
		t.Fatalf("%d posts, err %v", len(posts), err)
	}
}

func TestPreviewGivesUpWhenTheRequestEnds(t *testing.T) {
	src := &fakeSource{likes: aiLover(), pool: twoTopicPool(30), delay: time.Second}
	p, _, _ := tunedPersonal(t, src, newTunings(viewerDID, nil), nil)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, _, err := previewList(p, ctx, "for-you", viewerDID, Tuning{}, 10); !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("err %v", err)
	}
}

func TestPreviewErrors(t *testing.T) {
	ctx := context.Background()
	p, _, _ := tunedPersonal(t, &fakeSource{likes: aiLover(), pool: twoTopicPool(30)}, newTunings(viewerDID, nil), nil)
	if _, _, err := previewList(p, ctx, "nope", viewerDID, Tuning{}, 10); !errors.Is(err, errNotReady) {
		t.Errorf("unknown feed: %v", err)
	}
	if _, _, err := previewList(p, ctx, "for-you", viewerDID, Tuning{Freshness: "frozen"}, 10); err == nil || errors.Is(err, errNotReady) {
		t.Errorf("invalid draft: %v", err)
	}

	noPosts := &fakeSource{likes: aiLover(), poolErr: errors.New("clickhouse is down")}
	p, _, _ = tunedPersonal(t, noPosts, newTunings(viewerDID, nil), nil)
	if _, _, err := previewList(p, ctx, "for-you", viewerDID, Tuning{}, 10); !errors.Is(err, errNotReady) {
		t.Errorf("posts not read yet: %v", err)
	}

	broken := &fakeSource{likesErr: errors.New("likes unreadable"), pool: twoTopicPool(30)}
	p, _, _ = tunedPersonal(t, broken, newTunings(viewerDID, nil), nil)
	if _, _, err := previewList(p, ctx, "for-you", viewerDID, Tuning{}, 10); err == nil {
		t.Error("a viewer who couldn't be read was previewed")
	}
}

func TestTuningUnderConcurrentUse(t *testing.T) {
	pool := threeTopicPool(100)
	src := &fakeSource{likesFor: halfLifeLikes, pool: pool}
	p, _, _ := tunedPersonal(t, src, newTunings(viewerDID, nil), func(c *PersonalConfig) { c.MaxServes = 20 })
	ctx, cancel := context.WithTimeout(context.Background(), 300*time.Millisecond)
	defer cancel()
	tunings := []Tuning{
		{}, {HalfLifeDays: 1}, {Topics: map[string]float64{ai: 0}}, {Freshness: FreshnessFresh, HidePromo: true},
		{Topics: map[string]float64{bball: 2}, HalfLifeDays: 3}, {AuthorGap: ptr(0)},
	}
	var wg sync.WaitGroup
	for g := range 3 {
		wg.Add(3)
		go func() {
			defer wg.Done()
			for i := 0; ctx.Err() == nil; i++ {
				if err := p.SetTuning(ctx, viewerDID, tunings[(i+g)%len(tunings)]); err != nil && ctx.Err() == nil {
					t.Errorf("save: %v", err)
					return
				}
			}
		}()
		go func() {
			defer wg.Done()
			cursor := ""
			for ctx.Err() == nil {
				pg, err := p.Page(context.Background(), "for-you", viewerDID, cursor, 10)
				if err != nil {
					t.Errorf("page: %v", err)
					return
				}
				cursor = pg.Cursor
			}
		}()
		go func() {
			defer wg.Done()
			for i := 0; ctx.Err() == nil; i++ {
				if _, _, err := previewList(p, ctx, "for-you", viewerDID, tunings[(i+g)%len(tunings)], 10); err != nil && ctx.Err() == nil {
					t.Errorf("preview: %v", err)
					return
				}
			}
		}()
	}
	wg.Wait()
}

// previewList is Preview's posts, state and error: most tests only look at those.
func previewList(p *Personal, ctx context.Context, rkey, did string, draft Tuning, limit int) ([]Post, string, error) {
	r, err := p.Preview(ctx, rkey, did, draft, limit)
	return r.Posts, r.State, err
}
