package feedgen

import (
	"context"
	"math"
	"strings"
	"testing"

	"github.com/haileyok/topic-feed/internal/taxonomy"
	"gopkg.in/yaml.v3"
)

// withLikes is n posts of a topic, the first `liked` of them with this many likes and the rest
// with none: new posts nobody has reacted to yet.
func withLikes(topic string, n, liked int, likes uint64) []Post {
	ps := topicPosts(topic, n)
	for i := range ps {
		ps[i].Likes = 0
		if i < liked {
			ps[i].Likes = likes
		}
	}
	return ps
}

func minimum(v float64) func(*PersonalConfig) {
	return func(c *PersonalConfig) { c.MinEngagement = &v; *c.AuthorGap = 0 }
}

// A topic that has run out of posts people have reacted to doesn't make up the difference with
// posts they haven't: its slots go to the other topics.
func TestMinEngagementLeavesOutPostsNobodyHasReactedTo(t *testing.T) {
	pool := map[string][]Post{
		ai:   withLikes(ai, 26, 6, 10), // 6 reacted to, 20 not
		cats: withLikes(cats, 40, 40, 10),
	}
	p, _, _ := newPersonalFor(t, &fakeSource{likes: aiLover(), pool: pool}, minimum(5))
	got := postsOf(pool, firstURIs(t, p, viewerDID, 30))

	if len(got) != 30 {
		t.Fatalf("%d posts, want a full page: the other topic has plenty", len(got))
	}
	for _, post := range got {
		if e := post.Engagement(DefaultRanking.Weights); e < 5 {
			t.Errorf("%s has engagement %v, below the minimum of 5", post.URI, e)
		}
	}
	// The viewer likes AI three times as much as cats, so without the minimum AI would have most of
	// the slots. Only six of its posts qualify, and the cats take the rest.
	if c := topicCounts(got); c[ai] != 6 || c[cats] != 24 {
		t.Errorf("topics %v, want all six qualifying AI posts and cats for the other 24", c)
	}
}

// When nothing anywhere qualifies the feed is empty, not padded, and it is still the viewer's own
// feed rather than the welcome post.
func TestMinEngagementEndsTheFeedWhenNothingQualifies(t *testing.T) {
	pool := map[string][]Post{ai: withLikes(ai, 20, 0, 0), cats: withLikes(cats, 20, 0, 0)}
	p, _, _ := newPersonalFor(t, &fakeSource{likes: aiLover(), pool: pool}, minimum(5))
	pg := waitPersonal(t, p, viewerDID, 30)
	if pg.State != StatePersonal || len(pg.Items) != 0 || pg.Cursor != "" {
		t.Errorf("%+v, want an empty page of the viewer's own feed with nowhere further to go", pg)
	}
}

// The minimum counts reactions at the feed's weights. A viewer whose own ranking makes every kind
// count for nothing would otherwise have no post able to meet it.
func TestMinEngagementIsMeasuredAtTheFeedsWeightsNotTheViewers(t *testing.T) {
	pool := map[string][]Post{ai: withLikes(ai, 20, 10, 10)}
	zero := Tuning{Ranking: &RankingTuning{Like: ptr(0.0), Repost: ptr(0.0), Reply: ptr(0.0), Quote: ptr(0.0)}}
	p, _, _ := tunedPersonal(t, &fakeSource{likes: aiLover(), pool: pool}, newTunings(viewerDID, &zero), minimum(5))
	if got := firstURIs(t, p, viewerDID, 30); len(got) != 10 {
		t.Errorf("%d posts, want the ten that have been reacted to", len(got))
	}
}

func TestViewerCanChangeTheMinimumEngagement(t *testing.T) {
	pool := map[string][]Post{ai: withLikes(ai, 20, 10, 10)} // ten with 10 likes, ten with none
	shown := func(tune Tuning) int {
		p, _, _ := tunedPersonal(t, &fakeSource{likes: aiLover(), pool: pool}, newTunings(viewerDID, &tune), minimum(5))
		return len(firstURIs(t, p, viewerDID, 30))
	}
	for name, tc := range map[string]struct {
		tune Tuning
		want int
	}{
		"the feed's own minimum of 5":  {Tuning{}, 10},
		"none at all (0 is a value)":   {Tuning{MinEngagement: ptr(0.0)}, 20},
		"exactly what the posts have":  {Tuning{MinEngagement: ptr(10.0)}, 10},
		"a little more than they have": {Tuning{MinEngagement: ptr(10.5)}, 0},
	} {
		if got := shown(tc.tune); got != tc.want {
			t.Errorf("%s: %d posts, want %d", name, got, tc.want)
		}
	}
}

// The preview on the tuning page shows what the feed would pick, so it follows the same rule.
func TestPreviewLeavesOutPostsBelowTheMinimumEngagement(t *testing.T) {
	pool := map[string][]Post{ai: withLikes(ai, 20, 10, 10)}
	p, _, _ := newPersonalFor(t, &fakeSource{likes: aiLover(), pool: pool}, minimum(5))
	preview := func(draft Tuning) int {
		r, err := p.Preview(context.Background(), "for-you", viewerDID, draft, 20)
		if err != nil {
			t.Fatal(err)
		}
		return len(r.Posts)
	}
	if got := preview(Tuning{}); got != 10 {
		t.Errorf("the feed's minimum: %d posts, want 10", got)
	}
	if got := preview(Tuning{MinEngagement: ptr(0.0)}); got != 20 {
		t.Errorf("a draft with no minimum: %d posts, want 20", got)
	}
	if got := preview(Tuning{MinEngagement: ptr(11.0)}); got != 0 {
		t.Errorf("a draft asking for 11: %d posts, want none", got)
	}
}

func TestMinEngagementOfATuning(t *testing.T) {
	for name, tc := range map[string]struct {
		v    float64
		good bool
	}{
		"zero is allowed":       {0, true},
		"the most":              {MaxMinEngagement, true},
		"a fraction":            {0.5, true},
		"negative":              {-1, false},
		"more than the most":    {MaxMinEngagement + 1, false},
		"not a number":          {math.NaN(), false},
		"infinity":              {math.Inf(1), false},
		"negative infinity":     {math.Inf(-1), false},
		"just under the limit":  {MaxMinEngagement - 0.001, true},
		"just over the limit":   {MaxMinEngagement + 0.001, false},
		"a tiny negative value": {-0.001, false},
	} {
		err := Tuning{MinEngagement: ptr(tc.v)}.check()
		if tc.good && err != nil {
			t.Errorf("%s: %v", name, err)
		}
		if !tc.good && (err == nil || !strings.Contains(err.Error(), "minEngagement")) {
			t.Errorf("%s: %v, want an error naming minEngagement", name, err)
		}
	}
	if (Tuning{MinEngagement: ptr(0.0)}).IsZero() {
		t.Error("a minimum of zero changes the feed's own, so the tuning isn't empty")
	}
	if !(Tuning{}).IsZero() {
		t.Error("a tuning with nothing set is empty")
	}

	base := PersonalConfigDefaults()
	if got := *(Tuning{}).Config(base).MinEngagement; got != DefaultMinEngagement {
		t.Errorf("untuned: %v, want the feed's %v", got, DefaultMinEngagement)
	}
	if got := *(Tuning{MinEngagement: ptr(0.0)}).Config(base).MinEngagement; got != 0 {
		t.Errorf("a viewer's zero: %v, want 0", got)
	}
	if got := *(Tuning{MinEngagement: ptr(12.0)}).Config(base).MinEngagement; got != 12 {
		t.Errorf("a viewer's 12: %v", got)
	}
	// What is done to a tuned configuration never reaches the feed's own.
	c := (Tuning{}).Config(base)
	*c.MinEngagement = 99
	if *base.MinEngagement != DefaultMinEngagement {
		t.Errorf("the feed's minimum was changed through a copy: %v", *base.MinEngagement)
	}
}

func TestMinEngagementSettingOfAFeed(t *testing.T) {
	tax, err := taxonomy.Load("../../taxonomy/v1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	paths := TaxonomyPaths(tax)
	load := func(personal string) (*PersonalConfig, error) {
		var c Config
		if err := yaml.Unmarshal([]byte("feeds:\n  - rkey: for-you\n    display_name: For you\n    personal: "+personal+"\n"), &c); err != nil {
			t.Fatal(err)
		}
		if err := c.Validate(paths); err != nil {
			return nil, err
		}
		return c.Feeds[0].Personal, nil
	}

	if p, err := load("{}"); err != nil || p.MinEngagement == nil || *p.MinEngagement != 5 {
		t.Errorf("default: %+v, %v", p, err)
	}
	if p, err := load("{min_engagement: 12.5}"); err != nil || *p.MinEngagement != 12.5 {
		t.Errorf("set: %+v, %v", p, err)
	}
	// Zero is a value, not "unset": it turns the minimum off and must not become the default.
	if p, err := load("{min_engagement: 0}"); err != nil || p.MinEngagement == nil || *p.MinEngagement != 0 {
		t.Errorf("zero: %+v, %v", p, err)
	}
	for _, bad := range []string{"{min_engagement: 201}", "{min_engagement: -1}"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), "min_engagement") {
			t.Errorf("%s: %v, want an error naming min_engagement", bad, err)
		}
	}
}
