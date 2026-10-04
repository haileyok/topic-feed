package feedgen

import (
	"context"
	"slices"
	"testing"
	"time"
)

// Tests of the settings a viewer can make about their own feed, beyond which topics they like:
// ranking, filters on what the model scored, and how the feed is put together.

const wantNone = -1

// firstURIs are the posts of a viewer's first page, in order.
func firstURIs(t *testing.T, p *Personal, did string, limit int) []string {
	t.Helper()
	return itemURIs(page(t, p, did, "", limit))
}

func TestPersonalRankingOfTheViewersOwn(t *testing.T) {
	old, fresh := oldAndFresh()
	pool := map[string][]Post{ai: {old, fresh}}
	first := func(tune Tuning) string {
		p, _, _ := tunedPersonal(t, &fakeSource{likes: aiLover(), pool: pool}, newTunings(viewerDID, &tune), func(c *PersonalConfig) { *c.AuthorGap = 0 })
		uris := firstURIs(t, p, viewerDID, 10)
		if len(uris) != 2 {
			t.Fatalf("%d posts", len(uris))
		}
		return uris[0]
	}
	if got := first(Tuning{}); got != old.URI {
		t.Fatalf("the feed's own ranking puts %s first, want the well-liked old post", got)
	}
	for name, tune := range map[string]Tuning{
		"a high gravity sinks old posts": {Ranking: &RankingTuning{Gravity: ptr(8.0)}},
		"likes that count for nothing":   {Ranking: &RankingTuning{Like: ptr(0.0)}},
		"fresh slots of every one":       {Ranking: &RankingTuning{FreshEvery: ptr(1)}},
	} {
		if got := first(tune); got != fresh.URI {
			t.Errorf("%s: %s first, want the new post", name, got)
		}
	}
	// Numbers that change nothing the post is ranked by change nothing.
	if got := first(Tuning{Ranking: &RankingTuning{PromoPenalty: ptr(3.0), Quote: ptr(0.0)}}); got != old.URI {
		t.Errorf("a promo penalty and quotes that count for nothing: %s first", got)
	}
	// Freshness picks a start, and the viewer's numbers go on from it.
	if got := first(Tuning{Freshness: FreshnessFresh, Ranking: &RankingTuning{Gravity: ptr(0.0), FreshEvery: ptr(0)}}); got != old.URI {
		t.Errorf("fresh, but with no gravity and no fresh slots: %s first, want the well-liked old post", got)
	}
}

func TestPersonalRankingOfOneViewerLeavesOthersAlone(t *testing.T) {
	old, fresh := oldAndFresh()
	pool := map[string][]Post{ai: {old, fresh}}
	tun := newTunings(viewerDID, &Tuning{Ranking: &RankingTuning{Gravity: ptr(8.0)}})
	p, _, _ := tunedPersonal(t, &fakeSource{likes: aiLover(), pool: pool}, tun, func(c *PersonalConfig) { *c.AuthorGap = 0 })
	if got := firstURIs(t, p, viewerDID, 10); got[0] != fresh.URI {
		t.Fatalf("the viewer's own ranking: %v", got)
	}
	if got := firstURIs(t, p, "did:plc:someoneelse", 10); got[0] != old.URI {
		t.Errorf("another viewer's feed was ranked by someone else's numbers: %v", got)
	}
	// The ranked pools everyone shares are as they were.
	pools := p.feeds["for-you"].snapshot()
	for f, ranked := range pools {
		if ranked[ai][0].URI != old.URI && f != FreshnessFresh {
			t.Errorf("the %s pool was reordered: %s first", f, ranked[ai][0].URI)
		}
	}
	if pool[ai][0].Score != 0 || pool[ai][1].Score != 0 {
		t.Error("the posts the pool was made from were scored in place")
	}
}

func TestPersonalToneAndSignalBoosts(t *testing.T) {
	at := now.Add(-time.Hour)
	// Equal but for what the model scored; by default the post with the larger address goes first.
	plain := Post{URI: "at://did:plc:b/app.bsky.feed.post/1", DID: "did:plc:b", IndexedAt: at, TopPath: ai, TopPathP: 0.9,
		Tone: map[string]float32{"humorous": 0, "informative": 1}, Signals: map[string]float32{"substance": 0.5}}
	funny := Post{URI: "at://did:plc:a/app.bsky.feed.post/1", DID: "did:plc:a", IndexedAt: at, TopPath: ai, TopPathP: 0.9,
		Tone: map[string]float32{"humorous": 1, "informative": 0}, Signals: map[string]float32{"substance": 0.5}}
	pool := map[string][]Post{ai: {plain, funny}}
	first := func(tune Tuning) string {
		p, _, _ := tunedPersonal(t, &fakeSource{likes: aiLover(), pool: pool}, newTunings(viewerDID, &tune), func(c *PersonalConfig) { *c.AuthorGap = 0 })
		return firstURIs(t, p, viewerDID, 10)[0]
	}
	if got := first(Tuning{}); got != plain.URI {
		t.Fatalf("by default: %s first", got)
	}
	if got := first(Tuning{Tone: Rules{Weights: map[string]float64{"humorous": 3}}}); got != funny.URI {
		t.Errorf("a boost on funny: %s first", got)
	}
	if got := first(Tuning{Tone: Rules{Weights: map[string]float64{"informative": -3}}}); got != funny.URI {
		t.Errorf("a boost against informative: %s first", got)
	}
	// A boost on a score both have changes nothing between them.
	if got := first(Tuning{Signals: Rules{Weights: map[string]float64{"substance": 3}}}); got != plain.URI {
		t.Errorf("a boost on substance, which both have equally: %s first", got)
	}
	// Cutoffs on tone leave posts out rather than moving them.
	p, _, _ := tunedPersonal(t, &fakeSource{likes: aiLover(), pool: pool}, newTunings(viewerDID, &Tuning{Tone: Rules{Min: map[string]float32{"humorous": 0.5}}}),
		func(c *PersonalConfig) { *c.AuthorGap = 0 })
	if got := firstURIs(t, p, viewerDID, 10); len(got) != 1 || got[0] != funny.URI {
		t.Errorf("only funny posts: %v", got)
	}
}

func TestPersonalFeedSettingsOfTheViewer(t *testing.T) {
	pool := twoTopicPool(100)
	feed := func(tune Tuning, tweak func(*PersonalConfig)) (*Personal, PersonalPage) {
		p, _, _ := tunedPersonal(t, &fakeSource{likes: aiLover(), pool: pool}, newTunings(viewerDID, &tune), func(c *PersonalConfig) {
			*c.AuthorGap = 0
			if tweak != nil {
				tweak(c)
			}
		})
		return p, page(t, p, viewerDID, "", 300)
	}

	_, pg := feed(Tuning{ListSize: 7}, nil)
	if len(pg.Items) != 7 {
		t.Errorf("a list of 7: %d posts", len(pg.Items))
	}
	_, pg = feed(Tuning{ListSize: 900}, func(c *PersonalConfig) { c.ListSize = 20 })
	if len(pg.Items) != 20 {
		t.Errorf("a list longer than the feed's own can't be had: %d posts, want the feed's 20", len(pg.Items))
	}

	_, pg = feed(Tuning{Interests: 1}, nil)
	if got := mixOf(pool, pg); len(got) != 1 || got[ai] == 0 || pg.State != StatePersonal {
		t.Errorf("one interest: %v %s", got, pg.State)
	}
	_, pg = feed(Tuning{}, nil)
	if got := mixOf(pool, pg); len(got) != 2 {
		t.Errorf("both interests: %v", got)
	}

	// With more likes asked for than they have, they get a mix of every topic.
	_, pg = feed(Tuning{MinLikes: 100}, nil)
	if pg.State != StateGeneric {
		t.Errorf("a viewer with fewer likes than the minimum they set: %s, want a mix", pg.State)
	}
	_, pg = feed(Tuning{MinLikes: 40}, nil)
	if pg.State != StatePersonal {
		t.Errorf("a minimum equal to their likes: %s", pg.State)
	}
	_, pg = feed(Tuning{MinLikes: 2}, func(c *PersonalConfig) { c.MinLikes = 100 })
	if pg.State != StatePersonal {
		t.Errorf("fewer likes asked for than the feed does: %s", pg.State)
	}
}

func TestPersonalWindowAndTopicProbabilityOfTheViewer(t *testing.T) {
	mk := func(name string, age time.Duration, prob float32) Post {
		return Post{URI: "at://did:plc:" + name + "/app.bsky.feed.post/1", DID: "did:plc:" + name, IndexedAt: now.Add(-age),
			TopPath: ai, TopPathP: prob, TopPaths: []string{ai}, TopPs: []float32{prob}}
	}
	recent, older, unsure := mk("recent", time.Hour, 0.9), mk("older", 10*time.Hour, 0.9), mk("unsure", time.Hour, 0.6)
	pool := map[string][]Post{ai: {recent, older, unsure}}
	got := func(tune Tuning) []string {
		p, _, _ := tunedPersonal(t, &fakeSource{likes: aiLover(), pool: pool}, newTunings(viewerDID, &tune), func(c *PersonalConfig) { *c.AuthorGap = 0 })
		uris := firstURIs(t, p, viewerDID, 10)
		slices.Sort(uris)
		return uris
	}
	all := []string{recent.URI, older.URI, unsure.URI}
	slices.Sort(all)
	if g := got(Tuning{}); !slices.Equal(g, all) {
		t.Fatalf("untuned: %v", g)
	}
	if g := got(Tuning{WindowHours: 5}); slices.Contains(g, older.URI) || len(g) != 2 {
		t.Errorf("a window of 5 hours: %v", g)
	}
	if g := got(Tuning{WindowHours: 12}); !slices.Equal(g, all) {
		t.Errorf("a window of 12 hours: %v", g)
	}
	if g := got(Tuning{MinTopicProb: 0.8}); slices.Contains(g, unsure.URI) || len(g) != 2 {
		t.Errorf("topics the model is 80%% sure of: %v", g)
	}
	if g := got(Tuning{MinTopicProb: 0.55}); !slices.Equal(g, all) {
		t.Errorf("topics the model is 55%% sure of: %v", g)
	}
	if g := got(Tuning{WindowHours: 5, MinTopicProb: 0.8}); len(g) != 1 || g[0] != recent.URI {
		t.Errorf("both: %v", g)
	}
}

func TestPersonalMaxServesOfTheViewer(t *testing.T) {
	pool := map[string][]Post{ai: topicPosts(ai, 5)}
	twice := func(tune Tuning) (first, second int) {
		p, _, _ := tunedPersonal(t, &fakeSource{likes: aiLover(), pool: pool}, newTunings(viewerDID, &tune), func(c *PersonalConfig) { *c.AuthorGap = 0 })
		return len(firstURIs(t, p, viewerDID, 10)), len(firstURIs(t, p, viewerDID, 10))
	}
	if a, b := twice(Tuning{}); a != 5 || b != 5 {
		t.Errorf("the feed's own limit of two: %d then %d, want 5 then 5", a, b)
	}
	if a, b := twice(Tuning{MaxServes: 1}); a != 5 || b != 0 {
		t.Errorf("a limit of one: %d then %d, want 5 then none", a, b)
	}
	if a, b := twice(Tuning{MaxServes: 5}); a != 5 || b != 5 {
		t.Errorf("a limit of five: %d then %d", a, b)
	}
}

func TestPersonalShowSeenKeepsSeenPostsInTheFeed(t *testing.T) {
	pool := twoTopicPool(20) // 20 AI and 20 cat posts
	likes := aiLover()
	liked := pool[ai][0].URI
	likes.Liked[liked] = struct{}{}
	pool[ai][1].DID = viewerDID
	own := pool[ai][1].URI
	reported := pool["animals_nature/cats"][0].URI // Bluesky said the viewer saw it
	sentTwice := pool[ai][2].URI                   // the feed already sent it max_serves (2) times
	src := &fakeSource{likes: likes, pool: pool, seen: SeenData{
		Confirmed: map[string]struct{}{reported: {}},
		Serves:    map[string]int{sentTwice: 2},
	}}
	feed := func(tune Tuning) (*Personal, []string) {
		p, _, _ := tunedPersonal(t, src, newTunings(viewerDID, &tune), func(c *PersonalConfig) { *c.AuthorGap = 0 })
		return p, firstURIs(t, p, viewerDID, 100)
	}

	_, own1 := feed(Tuning{})
	if len(own1) != 36 || slices.Contains(own1, reported) || slices.Contains(own1, sentTwice) {
		t.Errorf("the feed's own rule: %d posts, want 36 (40 less liked, own, reported seen and sent twice)", len(own1))
	}

	p, got := feed(Tuning{ShowSeen: true})
	if len(got) != 38 {
		t.Fatalf("with seen posts shown: %d posts, want 38 (only liked and own left out)", len(got))
	}
	for _, u := range []string{reported, sentTwice} {
		if !slices.Contains(got, u) {
			t.Errorf("%s was seen but left out, though the viewer asked to keep seen posts", u)
		}
	}
	for _, u := range []string{liked, own} {
		if slices.Contains(got, u) {
			t.Errorf("%s (liked or the viewer's own) was shown", u)
		}
	}
	// Asking again and again, as a refresh does, wears nothing out: the feed never runs dry.
	for i := 2; i <= 4; i++ {
		if n := len(firstURIs(t, p, viewerDID, 100)); n != 38 {
			t.Errorf("request %d: %d posts, want 38 again", i, n)
		}
	}
	// A view Bluesky reports later is kept too.
	p.NoteInteractions(viewerDID, got[:5])
	if n := len(firstURIs(t, p, viewerDID, 100)); n != 38 {
		t.Errorf("after five more were reported seen: %d posts, want 38", n)
	}
}

func TestPersonalLookbackOfTheViewer(t *testing.T) {
	src := &fakeSource{likes: aiLover(), pool: twoTopicPool(50)}
	tun := newTunings(viewerDID, nil)
	p, _, _ := tunedPersonal(t, src, tun, nil)
	day := 24 * time.Hour
	page(t, p, viewerDID, "", 5)
	if got := src.lookbackDurations(); len(got) != 1 || got[0] != 30*day {
		t.Fatalf("the feed's own lookback: %v", got)
	}
	if err := p.SetTuning(context.Background(), viewerDID, Tuning{LookbackDays: 7}); err != nil {
		t.Fatal(err)
	}
	if got := src.lookbackDurations(); len(got) != 2 || got[1] != 7*day {
		t.Errorf("their likes weren't read again over 7 days: %v", got)
	}
	// Settings that don't change how likes are read don't read them again.
	if err := p.SetTuning(context.Background(), viewerDID, Tuning{LookbackDays: 7, Interests: 3, MinLikes: 2, ListSize: 30, MaxServes: 1}); err != nil {
		t.Fatal(err)
	}
	if got := src.lookbackDurations(); len(got) != 2 {
		t.Errorf("likes were read again for settings that don't touch them: %v", got)
	}
	if err := p.SetTuning(context.Background(), viewerDID, Tuning{}); err != nil {
		t.Fatal(err)
	}
	if got := src.lookbackDurations(); len(got) != 3 || got[2] != 30*day {
		t.Errorf("back to the feed's lookback: %v", got)
	}

	// A viewer arriving with a lookback already saved has their likes read that way.
	src2 := &fakeSource{likes: aiLover(), pool: twoTopicPool(50)}
	p2, _, _ := tunedPersonal(t, src2, newTunings(viewerDID, &Tuning{LookbackDays: 3}), nil)
	page(t, p2, viewerDID, "", 5)
	if got := src2.lookbackDurations(); len(got) != 2 || got[0] != 30*day || got[1] != 3*day {
		t.Errorf("a saved lookback: %v, want the feed's and then 3 days", got)
	}
}

func TestPreviewTellsWhatShareEachInterestGets(t *testing.T) {
	pool := twoTopicPool(60)
	p, _, _ := tunedPersonal(t, &fakeSource{likes: aiLover(), pool: pool}, newTunings(viewerDID, nil), nil)
	shares := func(draft Tuning) map[string]float64 {
		r, err := p.Preview(context.Background(), "for-you", viewerDID, draft, 10)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]float64{}
		for _, x := range r.Interests {
			out[x.Path] = x.Share
		}
		return out
	}
	if got := shares(Tuning{}); !near(got[ai], 0.75) || !near(got[cats], 0.25) || len(got) != 2 {
		t.Errorf("untuned: %v", got)
	}
	if got := shares(Tuning{Topics: map[string]float64{cats: 3}}); !near(got[ai], 0.5) || !near(got[cats], 0.5) {
		t.Errorf("cats turned up: %v", got)
	}
	if got := shares(Tuning{Topics: map[string]float64{cats: 0}}); !near(got[ai], 1) || len(got) != 1 {
		t.Errorf("cats muted: %v", got)
	}
	if got := shares(Tuning{Interests: 1}); !near(got[ai], 1) || len(got) != 1 {
		t.Errorf("one interest: %v", got)
	}
	if got := shares(Tuning{Topics: map[string]float64{bball: 2}}); len(got) != 3 || got[bball] <= got[cats] {
		t.Errorf("a topic added: %v", got)
	}
}

func TestPreviewUsesTheDraftsOwnRankingAndFeedSettings(t *testing.T) {
	old, fresh := oldAndFresh()
	p, _, _ := tunedPersonal(t, &fakeSource{likes: aiLover(), pool: map[string][]Post{ai: {old, fresh}}}, newTunings(viewerDID, nil),
		func(c *PersonalConfig) { *c.AuthorGap = 0 })
	preview := func(draft Tuning) PreviewResult {
		r, err := p.Preview(context.Background(), "for-you", viewerDID, draft, 10)
		if err != nil {
			t.Fatal(err)
		}
		return r
	}
	if r := preview(Tuning{}); r.Posts[0].URI != old.URI {
		t.Errorf("untuned: %s first", r.Posts[0].URI)
	}
	r := preview(Tuning{Ranking: &RankingTuning{Gravity: ptr(8.0)}})
	if r.Posts[0].URI != fresh.URI || r.Posts[0].Score <= r.Posts[1].Score || r.Posts[1].Score <= 0 {
		t.Errorf("a high gravity: %+v", r.Posts)
	}
	if r := preview(Tuning{WindowHours: 5}); len(r.Posts) != 1 || r.Posts[0].URI != fresh.URI {
		t.Errorf("a window of 5 hours: %+v", r.Posts)
	}
	if r := preview(Tuning{ListSize: 1}); len(r.Posts) != 1 {
		t.Errorf("a list of one: %d posts", len(r.Posts))
	}
	// Nothing was saved or recorded by asking.
	if !p.Tunings.(*fakeTunings).get(viewerDID).IsZero() {
		t.Error("a preview saved its draft")
	}
}
