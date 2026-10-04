package feedgen

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// countingBuilder is a Builder that says what it was asked and can be slowed, failed or held.
type countingBuilder struct {
	mu      sync.Mutex
	builds  map[string]int  // by Feed.Key
	seen    map[string]Feed // the feed as last asked for, by Feed.Key
	delay   time.Duration
	fail    map[string]error
	hold    chan struct{} // when set, a build waits for it to be closed
	running atomic.Int32
	peak    atomic.Int32
}

func newCountingBuilder() *countingBuilder {
	return &countingBuilder{builds: map[string]int{}, seen: map[string]Feed{}, fail: map[string]error{}}
}

func (b *countingBuilder) Build(ctx context.Context, f Feed, since time.Time, limit int) ([]Post, Removed, error) {
	n := b.running.Add(1)
	defer b.running.Add(-1)
	for {
		p := b.peak.Load()
		if n <= p || b.peak.CompareAndSwap(p, n) {
			break
		}
	}
	b.mu.Lock()
	b.builds[f.Key()]++
	b.seen[f.Key()] = f
	err := b.fail[f.Key()]
	hold, delay := b.hold, b.delay
	b.mu.Unlock()
	if hold != nil {
		select {
		case <-hold:
		case <-ctx.Done():
			return nil, Removed{}, ctx.Err()
		}
	}
	if delay > 0 {
		select {
		case <-time.After(delay):
		case <-ctx.Done():
			return nil, Removed{}, ctx.Err()
		}
	}
	if err != nil {
		return nil, Removed{}, err
	}
	now := time.Now()
	return []Post{
		{URI: "at://did:plc:a/app.bsky.feed.post/1-" + f.Key(), DID: "did:plc:a", IndexedAt: now.Add(-time.Hour), TopPath: "technology/ai", TopPathP: 0.9},
		{URI: "at://did:plc:b/app.bsky.feed.post/2-" + f.Key(), DID: "did:plc:b", IndexedAt: now.Add(-2 * time.Hour), TopPath: "technology/ai", TopPathP: 0.8},
	}, Removed{}, nil
}

func (b *countingBuilder) count(key string) int {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.builds[key]
}

func (b *countingBuilder) fed(key string) Feed {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.seen[key]
}

func (b *countingBuilder) setHold(h chan struct{}) {
	b.mu.Lock()
	b.hold = h
	b.mu.Unlock()
}

func (b *countingBuilder) setFail(key string, err error) {
	b.mu.Lock()
	b.fail[key] = err
	b.mu.Unlock()
}

var quietLog = slog.New(slog.NewTextHandler(io.Discard, nil))

func ownerFeed(rkey string) Feed {
	return Feed{Rkey: rkey, DisplayName: rkey, Paths: []string{"technology/ai"}, MinProb: 0.5, Ranking: DefaultRanking}
}

func personFeed(did, rkey string) Feed {
	f := ownerFeed(rkey)
	f.Owner = did
	return f
}

func newDynamic(t *testing.T, b Builder, feeds ...Feed) *Feeds {
	t.Helper()
	fs := NewFeeds(&Config{Feeds: feeds}, b, quietLog, 24*time.Hour, 20*time.Millisecond, 100)
	fs.UserEvery, fs.Idle, fs.BuildWait = time.Minute, 10*time.Minute, 2*time.Second
	return fs
}

func startFeeds(t *testing.T, fs *Feeds) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	t.Cleanup(cancel)
	fs.Start(ctx)
}

func waitUntil(t *testing.T, what string, cond func() bool) {
	t.Helper()
	for i := 0; i < 400; i++ {
		if cond() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("timed out waiting for %s", what)
}

const bob, carol = "did:plc:bob", "did:plc:carol"

func TestTheOwnersFeedsAreBuiltAtStartAndKeptFresh(t *testing.T) {
	b := newCountingBuilder()
	fs := newDynamic(t, b, ownerFeed("ai"), personFeed(bob, "mine"))
	startFeeds(t, fs)
	if posts, _, ok := fs.Posts("ai"); !ok || len(posts) != 2 {
		t.Fatalf("the owner's feed is built when Start returns: %v %v", len(posts), ok)
	}
	if b.count("did:plc:bob/mine") != 0 {
		t.Error("someone else's feed isn't built until somebody asks for it")
	}
	waitUntil(t, "the owner's feed to be rebuilt on the timer", func() bool { return b.count("ai") >= 3 })
	time.Sleep(60 * time.Millisecond)
	if b.count("did:plc:bob/mine") != 0 {
		t.Error("nor does the timer build it")
	}
}

func TestSomeoneElsesFeedIsBuiltWhenFirstAskedFor(t *testing.T) {
	b := newCountingBuilder()
	fs := newDynamic(t, b, ownerFeed("ai"), personFeed(bob, "mine"))
	startFeeds(t, fs)
	items, next, ready, err := fs.Page(context.Background(), bob+"/mine", "", 10)
	if err != nil || !ready || len(items) != 2 || next != "" {
		t.Fatalf("%v %q %v %v", items, next, ready, err)
	}
	if b.count(bob+"/mine") != 1 {
		t.Errorf("built %d times", b.count(bob+"/mine"))
	}
	if got := b.fed(bob + "/mine"); got.Owner != bob || got.Rkey != "mine" {
		t.Errorf("the builder is given the feed with its owner: %+v", got)
	}
	// Asked again, it is served from memory.
	if _, _, ready, _ := fs.Page(context.Background(), bob+"/mine", "", 10); !ready || b.count(bob+"/mine") != 1 {
		t.Errorf("the second request builds nothing: %d", b.count(bob+"/mine"))
	}
}

func TestManyRequestsForAFeedNobodyHasAskedForShareOneBuild(t *testing.T) {
	b := newCountingBuilder()
	b.delay = 60 * time.Millisecond
	fs := newDynamic(t, b, personFeed(bob, "mine"))
	startFeeds(t, fs)
	var wg sync.WaitGroup
	var notReady atomic.Int32
	for i := 0; i < 25; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if items, _, ready, err := fs.Page(context.Background(), bob+"/mine", "", 10); err != nil || !ready || len(items) != 2 {
				notReady.Add(1)
			}
		}()
	}
	wg.Wait()
	if notReady.Load() != 0 || b.count(bob+"/mine") != 1 {
		t.Errorf("%d requests were not served, and the feed was built %d times", notReady.Load(), b.count(bob+"/mine"))
	}
}

func TestAFeedThatIsSlowToBuildIsNotReadyUntilItIs(t *testing.T) {
	b := newCountingBuilder()
	hold := make(chan struct{})
	b.setHold(hold)
	fs := newDynamic(t, b, personFeed(bob, "mine"))
	fs.BuildWait = 40 * time.Millisecond
	startFeeds(t, fs)
	start := time.Now()
	if _, _, ready, err := fs.Page(context.Background(), bob+"/mine", "", 10); ready || err != nil {
		t.Fatalf("not ready yet: %v %v", ready, err)
	}
	if took := time.Since(start); took > 500*time.Millisecond {
		t.Errorf("a request waited %v for a build", took)
	}
	// The build goes on without the request that started it, and a later request finds it done.
	close(hold)
	waitUntil(t, "the build to finish", func() bool {
		_, _, ready, _ := fs.Page(context.Background(), bob+"/mine", "", 10)
		return ready
	})
	if b.count(bob+"/mine") != 1 {
		t.Errorf("one build served them all: %d", b.count(bob+"/mine"))
	}
}

func TestARequestThatGivesUpDoesNotStopTheBuildOthersAreWaitingFor(t *testing.T) {
	b := newCountingBuilder()
	hold := make(chan struct{})
	b.setHold(hold)
	fs := newDynamic(t, b, personFeed(bob, "mine"))
	startFeeds(t, fs)
	gone, cancel := context.WithCancel(context.Background())
	done := make(chan bool)
	go func() {
		_, _, ready, _ := fs.Page(gone, bob+"/mine", "", 10)
		done <- ready
	}()
	waitUntil(t, "the build to begin", func() bool { return b.count(bob+"/mine") == 1 })
	cancel()
	if ready := <-done; ready {
		t.Error("a request that was cancelled can't have been served")
	}
	close(hold)
	waitUntil(t, "the feed to be built anyway", func() bool {
		_, built, _ := fs.Posts(bob + "/mine")
		return !built.IsZero()
	})
}

func TestAFailedBuildIsTriedAgainByTheNextRequest(t *testing.T) {
	b := newCountingBuilder()
	b.setFail(bob+"/mine", errors.New("clickhouse is down"))
	fs := newDynamic(t, b, personFeed(bob, "mine"))
	startFeeds(t, fs)
	if _, _, ready, err := fs.Page(context.Background(), bob+"/mine", "", 10); ready || err != nil {
		t.Fatalf("a failed build is 'not ready', not an error to the reader: %v %v", ready, err)
	}
	b.setFail(bob+"/mine", nil)
	items, _, ready, err := fs.Page(context.Background(), bob+"/mine", "", 10)
	if err != nil || !ready || len(items) != 2 {
		t.Errorf("the next request builds it again: %v %v %v", items, ready, err)
	}
	if b.count(bob+"/mine") != 2 {
		t.Errorf("%d builds", b.count(bob+"/mine"))
	}
}

func TestAFeedInUseIsRebuiltAndOneNobodyAsksForIsLetGo(t *testing.T) {
	b := newCountingBuilder()
	fs := newDynamic(t, b, personFeed(bob, "busy"), personFeed(bob, "quiet"))
	fs.UserEvery, fs.Idle = 30*time.Second, 5*time.Minute
	startFeeds(t, fs)
	ctx := context.Background()
	fs.Page(ctx, bob+"/busy", "", 10)
	fs.Page(ctx, bob+"/quiet", "", 10)
	built := time.Now()

	// Soon after, nothing is due.
	fs.tendOnce(built.Add(10 * time.Second))
	time.Sleep(30 * time.Millisecond)
	if b.count(bob+"/busy") != 1 || b.count(bob+"/quiet") != 1 {
		t.Fatalf("rebuilt too soon: %d %d", b.count(bob+"/busy"), b.count(bob+"/quiet"))
	}

	// A minute on, the one still being asked for is rebuilt; the other, asked for once, is not
	// asked for again (it was used at 'built', which is a minute ago: not yet idle).
	fs.Page(ctx, bob+"/busy", "", 10) // keeps it in use
	busy := fs.stateOf(bob + "/busy")
	busy.lastUsed.Store(built.Add(55 * time.Second).UnixNano())
	fs.stateOf(bob + "/quiet").lastUsed.Store(built.UnixNano())
	fs.tendOnce(built.Add(60 * time.Second))
	waitUntil(t, "the feed in use to be rebuilt", func() bool { return b.count(bob+"/busy") == 2 })
	waitUntil(t, "the other to be rebuilt too (it has not been idle long enough to be let go)", func() bool { return b.count(bob+"/quiet") == 2 })

	// Past Idle with nobody asking: let go, and not rebuilt.
	fs.tendOnce(built.Add(6 * time.Minute))
	time.Sleep(30 * time.Millisecond)
	if _, builtAt, ok := fs.Posts(bob + "/quiet"); !ok || !builtAt.IsZero() {
		t.Errorf("an idle feed's build is dropped but the feed stays known: ok=%v builtAt=%v", ok, builtAt)
	}
	if b.count(bob+"/quiet") != 2 {
		t.Errorf("an idle feed isn't rebuilt: %d", b.count(bob+"/quiet"))
	}
	// Asked for again, it is built again.
	if items, _, ready, _ := fs.Page(ctx, bob+"/quiet", "", 10); !ready || len(items) != 2 || b.count(bob+"/quiet") != 3 {
		t.Errorf("asked for again: %v %v %d", items, ready, b.count(bob+"/quiet"))
	}
}

func TestAskingForAFeedMarksItInUseEvenWhenItIsNotReady(t *testing.T) {
	b := newCountingBuilder()
	b.setFail(bob+"/mine", errors.New("down"))
	fs := newDynamic(t, b, personFeed(bob, "mine"))
	startFeeds(t, fs)
	st := fs.stateOf(bob + "/mine")
	if st.lastUsed.Load() != 0 {
		t.Fatal("not asked for yet")
	}
	fs.Page(context.Background(), bob+"/mine", "", 10) // not ready: the build failed
	if age := time.Since(time.Unix(0, st.lastUsed.Load())); age > time.Second {
		t.Errorf("asked for a moment ago, marked %v ago", age)
	}
}

func TestTheOwnersFeedsAreNeverLetGoOfAndIdleDoesNotTouchThem(t *testing.T) {
	b := newCountingBuilder()
	fs := newDynamic(t, b, ownerFeed("ai"))
	startFeeds(t, fs)
	fs.tendOnce(time.Now().Add(24 * time.Hour))
	if posts, _, _ := fs.Posts("ai"); len(posts) == 0 {
		t.Error("the owner's feed was let go")
	}
}

func TestOnlyAFewOfPeoplesFeedsAreBuiltAtOnce(t *testing.T) {
	b := newCountingBuilder()
	b.delay = 30 * time.Millisecond
	var feeds []Feed
	for i := 0; i < 12; i++ {
		feeds = append(feeds, personFeed(bob, fmt.Sprintf("f%d", i)))
	}
	fs := newDynamic(t, b, feeds...)
	fs.slots = make(chan struct{}, 3)
	fs.BuildWait = 5 * time.Second
	startFeeds(t, fs)
	var wg sync.WaitGroup
	for i := range feeds {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, _, ready, _ := fs.Page(context.Background(), bob+"/"+fmt.Sprintf("f%d", i), "", 10); !ready {
				t.Errorf("feed %d never became ready", i)
			}
		}()
	}
	wg.Wait()
	if peak := b.peak.Load(); peak > 3 || peak < 2 {
		t.Errorf("up to 3 builds at once, got a peak of %d", peak)
	}
}

func TestTwoPeopleCanUseTheSameRkey(t *testing.T) {
	b := newCountingBuilder()
	bobs, carols := personFeed(bob, "cats"), personFeed(carol, "cats")
	carols.Paths = []string{"animals_nature/cats"}
	fs := newDynamic(t, b, ownerFeed("cats"), bobs, carols)
	startFeeds(t, fs)
	fs.Page(context.Background(), bob+"/cats", "", 10)
	fs.Page(context.Background(), carol+"/cats", "", 10)
	if b.fed(carol + "/cats").Paths[0] != "animals_nature/cats" || b.fed(bob + "/cats").Paths[0] != "technology/ai" || b.fed("cats").Owner != "" {
		t.Errorf("each gets its own: %+v %+v %+v", b.fed("cats"), b.fed(bob+"/cats"), b.fed(carol+"/cats"))
	}
	if f, ok := fs.Lookup(carol + "/cats"); !ok || f.Owner != carol {
		t.Errorf("lookup: %+v %v", f, ok)
	}
	if _, ok := fs.Lookup("did:plc:nobody/cats"); ok {
		t.Error("an unknown feed")
	}
	if _, _, ok := fs.Posts("cats"); !ok {
		t.Error("the owner's feed is known by its rkey")
	}
}

func TestListIsTheOwnersFeedsInOrder(t *testing.T) {
	fs := newDynamic(t, newCountingBuilder(), ownerFeed("b"), personFeed(bob, "x"), ownerFeed("a"), personFeed(carol, "y"))
	var got []string
	for _, f := range fs.List() {
		got = append(got, f.Rkey)
	}
	if fmt.Sprint(got) != "[b a]" {
		t.Errorf("%v: the owner's, in the order given, and not anyone else's", got)
	}
	// What List returns is a copy: changing it does not change the feeds.
	l := fs.List()
	l[0].Rkey = "changed"
	if fs.List()[0].Rkey != "b" {
		t.Error("List returned the feeds themselves")
	}
}

func TestAskingForAFeedThatIsNotThereIsAnError(t *testing.T) {
	fs := newDynamic(t, newCountingBuilder(), ownerFeed("ai"))
	startFeeds(t, fs)
	if _, _, _, err := fs.Page(context.Background(), "nothing", "", 10); !errors.Is(err, errUnknownFeed) {
		t.Errorf("%v", err)
	}
	if _, _, ok := fs.Posts("nothing"); ok {
		t.Error("known")
	}
}

func TestReconcileAddsRemovesAndChangesFeeds(t *testing.T) {
	b := newCountingBuilder()
	ai := ownerFeed("ai")
	fs := newDynamic(t, b, ai, ownerFeed("gone"), personFeed(bob, "mine"))
	startFeeds(t, fs)
	fs.Page(context.Background(), bob+"/mine", "", 10)
	aiState, mineState := fs.stateOf("ai"), fs.stateOf(bob+"/mine")
	_, _, _ = fs.Posts("ai")

	changed := ownerFeed("ai")
	changed.MinProb = 0.9
	changedMine := personFeed(bob, "mine")
	changedMine.Description = "now with a description"
	fs.Reconcile([]Feed{ai, ownerFeed("new"), personFeed(carol, "fresh")})

	// A feed that is unchanged keeps its state (and so its builds); a removed one is gone.
	if fs.stateOf("ai") != aiState {
		t.Error("an unchanged feed was rebuilt from nothing")
	}
	if _, _, ok := fs.Posts("gone"); ok {
		t.Error("a removed feed is still served")
	}
	if _, _, ok := fs.Posts(bob + "/mine"); ok {
		t.Error("a removed feed of someone else's is still served")
	}
	// A new feed of the owner's is built at once, and kept fresh.
	waitUntil(t, "the new feed to be built", func() bool { p, _, ok := fs.Posts("new"); return ok && len(p) == 2 })
	n := b.count("new")
	waitUntil(t, "the new feed to be rebuilt on the timer", func() bool { return b.count("new") > n })
	// A new feed of someone else's is not.
	time.Sleep(50 * time.Millisecond)
	if b.count(carol+"/fresh") != 0 {
		t.Error("a new feed of someone else's was built before anybody asked")
	}

	// The removed feed's timer stops.
	gone := b.count("gone")
	time.Sleep(80 * time.Millisecond)
	if b.count("gone") != gone {
		t.Errorf("a removed feed is still being built (%d -> %d)", gone, b.count("gone"))
	}

	// A changed feed starts afresh with its new settings.
	fs.Reconcile([]Feed{changed, ownerFeed("new"), changedMine})
	if fs.stateOf("ai") == aiState {
		t.Error("a changed feed kept its old state")
	}
	waitUntil(t, "the changed feed to be built with its new settings", func() bool { return b.fed("ai").MinProb == 0.9 })
	fs.Reconcile([]Feed{changed, changedMine})
	if st := fs.stateOf(bob + "/mine"); st == mineState || st.feed.Description != "now with a description" {
		t.Error("a changed feed of someone else's starts afresh")
	}
	if f, _ := fs.Lookup("ai"); f.MinProb != 0.9 {
		t.Errorf("lookup gives the new settings: %+v", f)
	}
}

func TestReconcileBeforeStartBuildsNothingAndStartBuildsWhatThereIs(t *testing.T) {
	b := newCountingBuilder()
	fs := newDynamic(t, b, ownerFeed("old"))
	fs.Reconcile([]Feed{ownerFeed("a"), ownerFeed("b"), personFeed(bob, "x")})
	time.Sleep(30 * time.Millisecond)
	if b.count("a") != 0 || b.count("old") != 0 {
		t.Fatal("built before Start")
	}
	startFeeds(t, fs)
	if _, _, ok := fs.Posts("old"); ok {
		t.Error("the feed that was replaced is still known")
	}
	for _, k := range []string{"a", "b"} {
		if p, _, ok := fs.Posts(k); !ok || len(p) != 2 {
			t.Errorf("%s is built when Start returns", k)
		}
	}
}

func TestReconcileIgnoresASecondFeedWithTheSameKey(t *testing.T) {
	first, second := ownerFeed("ai"), ownerFeed("ai")
	second.MinProb = 0.99
	fs := newDynamic(t, newCountingBuilder(), first, second)
	if f, _ := fs.Lookup("ai"); f.MinProb != first.MinProb || len(fs.List()) != 1 {
		t.Errorf("%+v %d", f, len(fs.List()))
	}
}

func TestACursorFromABuildThatWasLetGoStillWorks(t *testing.T) {
	b := newCountingBuilder()
	fs := newDynamic(t, b, personFeed(bob, "mine"))
	fs.Idle = time.Minute
	startFeeds(t, fs)
	_, _, _, _ = fs.Page(context.Background(), bob+"/mine", "", 1)
	items, next, _, _ := fs.Page(context.Background(), bob+"/mine", "", 1)
	if len(items) != 1 || next == "" {
		t.Fatalf("%v %q", items, next)
	}
	fs.tendOnce(time.Now().Add(time.Hour)) // let go
	p, _, ready, err := fs.Page(context.Background(), bob+"/mine", next, 1)
	if err != nil || !ready || len(p) != 1 {
		t.Errorf("a reader part way through a feed that was let go goes on: %v %v %v", p, ready, err)
	}
}

func TestSyncFromKeepsTheFeedsAsTheDatabaseSaysAndSurvivesItsTroubles(t *testing.T) {
	b := newCountingBuilder()
	fs := newDynamic(t, b, ownerFeed("ai"), forYouFeed())
	startFeeds(t, fs)

	var mu sync.Mutex
	loaded := []Feed{ownerFeed("ai"), ownerFeed("added"), forYouFeed()}
	var loadErr error
	loads := 0
	load := func(context.Context) ([]Feed, error) {
		mu.Lock()
		defer mu.Unlock()
		loads++
		return loaded, loadErr
	}
	set := func(f []Feed, err error) {
		mu.Lock()
		loaded, loadErr = f, err
		mu.Unlock()
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go fs.SyncFrom(ctx, load, time.Hour) // only SyncNow makes it look
	has := func(k string) bool { _, _, ok := fs.Posts(k); return ok }

	fs.SyncNow()
	waitUntil(t, "the added feed to appear", func() bool { return has("added") })

	set(nil, errors.New("clickhouse is down"))
	fs.SyncNow()
	waitUntil(t, "the failed load to be tried", func() bool { mu.Lock(); defer mu.Unlock(); return loads >= 2 })
	time.Sleep(20 * time.Millisecond)
	if !has("ai") || !has("added") {
		t.Error("a load that failed took feeds away")
	}

	set([]Feed{forYouFeed()}, nil) // the database answered, with no topic feeds at all
	fs.SyncNow()
	waitUntil(t, "that load to be tried", func() bool { mu.Lock(); defer mu.Unlock(); return loads >= 3 })
	time.Sleep(20 * time.Millisecond)
	if !has("ai") || !has("added") {
		t.Error("serving no feeds at all is not an answer to believe")
	}

	set([]Feed{ownerFeed("added"), forYouFeed()}, nil)
	fs.SyncNow()
	waitUntil(t, "a feed to be removed", func() bool { return !has("ai") })
	if !has("added") {
		t.Error("the others stay")
	}
}
