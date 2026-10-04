package feedgen

import (
	"math"
	"strings"
	"testing"
	"time"

	"github.com/haileyok/topic-feed/internal/labelpolicy"
)

var inspectNow = time.Date(2026, 10, 4, 12, 0, 0, 0, time.UTC)

func testPolicy() *labelpolicy.Policy {
	return &labelpolicy.Policy{Labelers: []string{"did:plc:labeler"}, AdultOnly: []string{"porn", "sexual"}, Drop: []string{"csam"}}
}

// aiPost is a post the model is sure is about AI, an hour old, that nothing stands in the way of.
func aiPost() EvalInput {
	return EvalInput{
		Post: Post{
			URI: "at://did:plc:a/app.bsky.feed.post/1", DID: "did:plc:a", IndexedAt: inspectNow.Add(-time.Hour),
			TopPath: "technology/ai", TopPathP: 0.8, Tone: map[string]float32{"informative": 0.6, "outraged": 0.1},
			Signals: map[string]float32{"substance": 0.5, "general_interest": 0.4, "promo": 0.05, "spam": 0.01},
			Likes:   6, Reposts: 1,
		},
		Model: "v5", FeedPolicy: labelpolicy.OK,
		PathProbs:  map[string]float32{"technology/ai": 0.8, "technology/software_dev": 0.1},
		BroadProbs: map[string]float32{"technology": 0.9},
		Policy:     testPolicy(),
	}
}

func aiFeed() Feed {
	return Feed{Rkey: "ai", DisplayName: "AI", Paths: []string{"technology/ai"}, MinProb: 0.5, Ranking: DefaultRanking}
}

// failed is the names of the checks the post fails (age aside).
func failed(v FeedVerdict) []string {
	var out []string
	for _, c := range v.Checks {
		if !c.Pass && !c.Soft {
			out = append(out, c.Name)
		}
	}
	return out
}

func check(v FeedVerdict, name string) (Check, bool) {
	for _, c := range v.Checks {
		if c.Name == name {
			return c, true
		}
	}
	return Check{}, false
}

func TestEvaluateFeedRules(t *testing.T) {
	type tc struct {
		name    string
		feed    func(*Feed)
		in      func(*EvalInput)
		matches bool
		fails   []string // names of the checks that fail
		reason  string   // what the first failure says
	}
	for _, c := range []tc{
		{name: "a post that fits", matches: true},
		{name: "any topic takes a post about anything", feed: func(f *Feed) { f.Paths = []string{AnyTopic}; f.MinProb = 0.9 },
			in: func(i *EvalInput) { i.PathProbs = map[string]float32{} }, matches: true},
		{name: "a broad topic counts the broad probability", feed: func(f *Feed) { f.Paths = []string{"technology"} },
			in: func(i *EvalInput) {
				i.PathProbs = map[string]float32{"technology/ai": 0.2, "technology/software_dev": 0.2}
			}, matches: true},
		{name: "topic under the minimum", in: func(i *EvalInput) { i.PathProbs["technology/ai"] = 0.49 },
			fails: []string{"Topic"}, reason: "Topic: best match is technology/ai at 0.49, and the feed needs at least 0.5"},
		{name: "topic exactly at the minimum is in", in: func(i *EvalInput) { i.PathProbs["technology/ai"] = 0.5 }, matches: true},
		{name: "a hair under the minimum reads as one", in: func(i *EvalInput) { i.PathProbs["technology/ai"] = 0.4999 },
			fails: []string{"Topic"}, reason: "Topic: best match is technology/ai at 0.4999, and the feed needs at least 0.5"},
		{name: "topic the model didn't score", in: func(i *EvalInput) { i.PathProbs = map[string]float32{"sports/baseball": 0.9} },
			fails: []string{"Topic"}, reason: "none of the feed's topics (technology/ai) is among the topics the model scored"},
		{name: "the best of several paths counts", feed: func(f *Feed) { f.Paths = []string{"sports/baseball", "technology/ai"} }, matches: true},
		{name: "an excluded topic over its limit", feed: func(f *Feed) { f.Exclude = map[string]float32{"technology/software_dev": 0.05} },
			fails: []string{"Not about technology/software_dev"}, reason: "0.1, and the feed allows at most 0.05"},
		{name: "an excluded topic at its limit is in", feed: func(f *Feed) { f.Exclude = map[string]float32{"technology/software_dev": 0.1} }, matches: true},
		{name: "an excluded broad topic uses the broad probability", feed: func(f *Feed) { f.Exclude = map[string]float32{"technology": 0.5} },
			fails: []string{"Not about technology"}},
		{name: "tone over its maximum", feed: func(f *Feed) { f.Tone = Rules{Max: map[string]float32{"outraged": 0.05}} },
			fails: []string{"Tone: outraged at most 0.05"}},
		{name: "tone under its minimum", feed: func(f *Feed) { f.Tone = Rules{Min: map[string]float32{"informative": 0.7}} },
			fails: []string{"Tone: informative at least 0.7"}},
		{name: "a tone the post has no score for counts as zero", feed: func(f *Feed) { f.Tone = Rules{Min: map[string]float32{"humorous": 0.1}} },
			fails: []string{"Tone: humorous at least 0.1"}},
		{name: "a signal over its maximum", feed: func(f *Feed) { f.Signals = Rules{Max: map[string]float32{"promo": 0.01}} },
			fails: []string{"Signal: promo at most 0.01"}},
		{name: "a signal under its minimum", feed: func(f *Feed) { f.Signals = Rules{Min: map[string]float32{"substance": 0.9}} },
			fails: []string{"Signal: substance at least 0.9"}},
		{name: "never scored by the model", in: func(i *EvalInput) { i.Model = "" },
			fails: []string{"Scored by the topic model"}},
		{name: "the policy dropped it", in: func(i *EvalInput) { i.FeedPolicy = labelpolicy.Drop; i.Model = "" },
			fails: []string{"Scored by the topic model", "Label policy when it was processed"}},
		{name: "adult-only is out of a feed that doesn't allow it", in: func(i *EvalInput) { i.FeedPolicy = labelpolicy.AdultOnly },
			fails: []string{"Label policy when it was processed"}},
		{name: "adult-only is in a feed that allows it", feed: func(f *Feed) { f.AllowAdult = true },
			in: func(i *EvalInput) { i.FeedPolicy = labelpolicy.AdultOnly }, matches: true},
		{name: "deleted", in: func(i *EvalInput) { i.Deleted = true }, fails: []string{"Not deleted"}},
		{name: "the author lost their account", in: func(i *EvalInput) { i.AuthorInactive = true }, fails: []string{"Author's account is active"}},
		{name: "a label now on the author makes it adult-only", in: func(i *EvalInput) { i.CurrentLabels = []string{"porn"} },
			fails: []string{"Labels on the post and its author"}},
		{name: "a label now on the author is fine for an adult feed", feed: func(f *Feed) { f.AllowAdult = true },
			in: func(i *EvalInput) { i.CurrentLabels = []string{"porn"} }, matches: true},
		{name: "a label the policy drops is out everywhere", feed: func(f *Feed) { f.AllowAdult = true },
			in: func(i *EvalInput) { i.Post.Labels = []string{"csam"} }, fails: []string{"Labels on the post and its author"}},
		{name: "several things wrong are all reported, the first is the reason", in: func(i *EvalInput) { i.Deleted = true; i.PathProbs["technology/ai"] = 0.1 },
			fails: []string{"Topic", "Not deleted"}, reason: "Topic:"},
	} {
		t.Run(c.name, func(t *testing.T) {
			f, in := aiFeed(), aiPost()
			if c.feed != nil {
				c.feed(&f)
			}
			if c.in != nil {
				c.in(&in)
			}
			v := EvaluateFeed(f, in, inspectNow, 24*time.Hour)
			if v.Matches != c.matches {
				t.Errorf("Matches = %v, want %v; failed: %v", v.Matches, c.matches, failed(v))
			}
			got := failed(v)
			if len(got) != len(c.fails) {
				t.Fatalf("failed checks %v, want %v", got, c.fails)
			}
			for i := range got {
				if got[i] != c.fails[i] {
					t.Errorf("failed checks %v, want %v", got, c.fails)
				}
			}
			if c.reason != "" && !strings.Contains(v.Reason, c.reason) {
				t.Errorf("reason %q, want it to contain %q", v.Reason, c.reason)
			}
			if c.matches && v.Reason != "" {
				t.Errorf("a match with a reason: %q", v.Reason)
			}
			if !v.Fresh {
				t.Error("an hour-old post is within the window")
			}
		})
	}
}

func TestEvaluateFeedAgeIsSoft(t *testing.T) {
	f, in := aiFeed(), aiPost()
	in.Post.IndexedAt = inspectNow.Add(-30 * time.Hour)
	v := EvaluateFeed(f, in, inspectNow, 24*time.Hour)
	if !v.Matches || v.Fresh {
		t.Fatalf("an old post still matches, but isn't fresh: matches %v, fresh %v", v.Matches, v.Fresh)
	}
	c, ok := check(v, "Within the feed's window")
	if !ok || c.Pass || !c.Soft || !strings.Contains(c.Detail, "matched while it was fresh") {
		t.Errorf("the window check: %+v", c)
	}
	// Exactly at the edge is inside, as the SQL's indexed_at >= since says.
	in.Post.IndexedAt = inspectNow.Add(-24 * time.Hour)
	if v := EvaluateFeed(f, in, inspectNow, 24*time.Hour); !v.Fresh {
		t.Error("a post exactly a window old is still in")
	}
	in.Post.IndexedAt = inspectNow.Add(-24*time.Hour - time.Microsecond)
	if v := EvaluateFeed(f, in, inspectNow, 24*time.Hour); v.Fresh {
		t.Error("a microsecond older is out")
	}
}

func TestEvaluateFeedTopicMatchValue(t *testing.T) {
	f, in := aiFeed(), aiPost()
	if v := EvaluateFeed(f, in, inspectNow, 24*time.Hour); v.Match != 0.8 {
		t.Errorf("Match = %v, want 0.8", v.Match)
	}
	f.Paths = []string{AnyTopic}
	if v := EvaluateFeed(f, in, inspectNow, 24*time.Hour); v.Match != 1 {
		t.Errorf("any topic matches at 1, got %v", v.Match)
	}
}

func TestNumVsNeverReadsAsTheLimitUnlessEqual(t *testing.T) {
	for _, c := range []struct {
		v, limit float32
		want     string
	}{
		{0.4999, 0.5, "0.4999"}, {0.5, 0.5, "0.5"}, {0.5001, 0.5, "0.5001"}, {0.49, 0.5, "0.49"}, {0.8, 0.5, "0.8"},
		{0.123456, 0.123, "0.1235"}, {0, 0.5, "0"}, {1, 0.5, "1"},
	} {
		got := numVs(c.v, c.limit)
		if got != c.want {
			t.Errorf("numVs(%v, %v) = %q, want %q", c.v, c.limit, got, c.want)
		}
		if c.v != c.limit && got == num3(c.limit) {
			t.Errorf("numVs(%v, %v) = %q reads as the limit", c.v, c.limit, got)
		}
	}
}

func TestNum3(t *testing.T) {
	for in, want := range map[float32]string{0: "0", 1: "1", 0.5: "0.5", 0.8: "0.8", 0.49: "0.49", 0.4999: "0.5", 0.499: "0.499", 0.123456: "0.123"} {
		if got := num3(in); got != want {
			t.Errorf("num3(%v) = %q, want %q", in, got, want)
		}
	}
}

// The score the page explains is the score the feed ranks by.
func TestScoreBreakdownIsTheScoreRankingUses(t *testing.T) {
	f := aiFeed()
	f.Ranking = Ranking{Gravity: 1.7, PromoPenalty: 2, Weights: Weights{Like: 1, Repost: 2, Reply: 2, Quote: 3}, FreshEvery: 4, AuthorGap: 3}
	f.Tone = Rules{Weights: map[string]float64{"informative": 0.5}}
	f.Signals = Rules{Weights: map[string]float64{"news": -1}}
	for i, in := range []EvalInput{aiPost(), func() EvalInput { x := aiPost(); x.Post.Signals = map[string]float32{"promo": 1}; return x }(),
		func() EvalInput { x := aiPost(); x.Post.IndexedAt = inspectNow.Add(time.Hour); return x }()} {
		posts := []Post{in.Post}
		Score(posts, f, inspectNow)
		parts := ScoreBreakdown(in.Post, f, inspectNow)
		if posts[0].Score != parts.Score {
			t.Errorf("post %d: ranked at %v, explained as %v", i, posts[0].Score, parts.Score)
		}
		if want := (parts.Prior + parts.Engagement) / parts.Decay; math.Abs(want-parts.Score) > 1e-12 {
			t.Errorf("post %d: (prior + engagement) / decay = %v, score %v", i, want, parts.Score)
		}
		if parts.Prior < 0.1 {
			t.Errorf("post %d: prior %v is under the floor of 0.1", i, parts.Prior)
		}
	}
	// The pieces, by hand, for the plain post: prior 1 + 0.5 + 0.4 - 2*0.05 + 0.5*0.6 + 0 = 2.1; engagement 6 + 2 = 8; age 1 h.
	p := ScoreBreakdown(aiPost().Post, f, inspectNow)
	if math.Abs(p.Prior-2.1) > 1e-6 || p.Engagement != 8 || p.AgeHours != 1 || math.Abs(p.Decay-math.Pow(3, 1.7)) > 1e-12 {
		t.Errorf("the pieces: %+v", p)
	}
}

func TestEvaluateFeedRanksWithTheFeedsOwnWeights(t *testing.T) {
	f := aiFeed()
	in := aiPost()
	base := EvaluateFeed(f, in, inspectNow, 24*time.Hour).Ranking.Score
	f.Ranking.Weights.Like = 10
	if more := EvaluateFeed(f, in, inspectNow, 24*time.Hour).Ranking.Score; more <= base {
		t.Errorf("likes counting for more should raise the score: %v then %v", base, more)
	}
}

func forYouFeed() Feed {
	min := 5.0
	cfg := PersonalConfigDefaults()
	cfg.MinEngagement = &min
	return Feed{Rkey: "for-you", DisplayName: "For you", Ranking: DefaultRanking, Personal: &cfg}
}

func TestEvaluatePersonal(t *testing.T) {
	for _, c := range []struct {
		name    string
		in      func(*EvalInput)
		matches bool
		fails   []string
	}{
		{name: "a well-liked post about AI", matches: true},
		{name: "unclear topic", in: func(i *EvalInput) { i.Post.TopPath = "unclear" }, fails: []string{"Belongs to a subtopic"}},
		{name: "model not sure of the subtopic", in: func(i *EvalInput) { i.Post.TopPathP = 0.3 }, fails: []string{"Belongs to a subtopic"}},
		{name: "exactly as sure as the feed needs", in: func(i *EvalInput) { i.Post.TopPathP = 0.5 }, matches: true},
		{name: "nobody has reacted enough", in: func(i *EvalInput) { i.Post.Likes, i.Post.Reposts = 3, 0 }, fails: []string{"Enough reactions"}},
		{name: "reposts count double", in: func(i *EvalInput) { i.Post.Likes, i.Post.Reposts = 1, 2 }, matches: true},
		{name: "adult-only never goes in a personal pool", in: func(i *EvalInput) { i.FeedPolicy = labelpolicy.AdultOnly }, fails: []string{"Label policy when it was processed"}},
		{name: "a label now makes it adult-only", in: func(i *EvalInput) { i.CurrentLabels = []string{"sexual"} }, fails: []string{"Labels on the post and its author"}},
		{name: "deleted", in: func(i *EvalInput) { i.Deleted = true }, fails: []string{"Not deleted"}},
		{name: "never scored", in: func(i *EvalInput) { i.Model = "" }, fails: []string{"Scored by the topic model"}},
	} {
		t.Run(c.name, func(t *testing.T) {
			in := aiPost()
			if c.in != nil {
				c.in(&in)
			}
			v := EvaluatePersonal(forYouFeed(), in, inspectNow)
			if !v.Personal || v.Note == "" {
				t.Errorf("a personal verdict says so and notes that it depends on the viewer: %+v", v)
			}
			if v.Matches != c.matches {
				t.Errorf("Matches = %v, want %v; failed %v", v.Matches, c.matches, failed(v))
			}
			if got := failed(v); len(got) != len(c.fails) || (len(got) > 0 && got[0] != c.fails[0]) {
				t.Errorf("failed %v, want %v", got, c.fails)
			}
		})
	}
	// With no minimum configured there is no check for one.
	f := forYouFeed()
	f.Personal.MinEngagement = nil
	v := EvaluatePersonal(f, aiPost(), inspectNow)
	if _, ok := check(v, "Enough reactions"); ok {
		t.Error("no minimum, no check")
	}
}

func TestLiveStatusIn(t *testing.T) {
	mk := func(name string, ago time.Duration) Post {
		return Post{URI: "at://did:plc:" + name + "/app.bsky.feed.post/1", IndexedAt: inspectNow.Add(-ago)}
	}
	build := []Post{mk("x", time.Hour), mk("y", 2*time.Hour), mk("z", 5*time.Hour)}
	built := inspectNow.Add(-10 * time.Second)
	match := FeedVerdict{Matches: true, Fresh: true}

	ls := LiveStatusIn(build, built, build[1].URI, match, build[1].IndexedAt, 3000, 24*time.Hour)
	if !ls.InBuild || ls.Position != 2 || ls.Total != 3 || ls.Why != "" {
		t.Errorf("in the build: %+v", ls)
	}
	ls = LiveStatusIn(build, built, "at://did:plc:q/app.bsky.feed.post/1", match, inspectNow.Add(-9*time.Hour), 3, 24*time.Hour)
	if ls.InBuild || !strings.Contains(ls.Why, "keeps only the 3 newest") {
		t.Errorf("older than everything in a full feed: %+v", ls)
	}
	ls = LiveStatusIn(build, built, "at://did:plc:q/app.bsky.feed.post/1", match, inspectNow.Add(-90*time.Minute), 3000, 24*time.Hour)
	if ls.InBuild || !strings.Contains(ls.Why, "may have arrived after the build") {
		t.Errorf("newer than the oldest but missing: %+v", ls)
	}
	ls = LiveStatusIn(build, built, "at://did:plc:q/app.bsky.feed.post/1", FeedVerdict{Matches: true, Fresh: false}, inspectNow.Add(-30*time.Hour), 3000, 24*time.Hour)
	if ls.InBuild || !strings.Contains(ls.Why, "older than the 24h0m0s") {
		t.Errorf("past the window: %+v", ls)
	}
	ls = LiveStatusIn(build, built, "at://did:plc:q/app.bsky.feed.post/1", FeedVerdict{Matches: false}, inspectNow, 3000, 24*time.Hour)
	if ls.InBuild || ls.Why != "" {
		t.Errorf("not matching: the checks say why, not this: %+v", ls)
	}
	ls = LiveStatusIn(nil, time.Time{}, "x", match, inspectNow, 3000, 24*time.Hour)
	if ls.InBuild || !strings.Contains(ls.Why, "hasn't been built") {
		t.Errorf("before the first build: %+v", ls)
	}
	ls = LiveStatusIn(nil, built, "x", match, inspectNow, 3000, 24*time.Hour)
	if ls.InBuild || ls.Total != 0 || !strings.Contains(ls.Why, "arrived after the build") {
		t.Errorf("an empty build: %+v", ls)
	}
}
