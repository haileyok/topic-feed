package feedgen

import (
	"strings"
	"testing"
	"time"

	"github.com/haileyok/topic-feed/internal/taxonomy"
	"gopkg.in/yaml.v3"
)

func candidateURIs(cs []candidate) []string {
	out := make([]string, len(cs))
	for i, c := range cs {
		out[i] = c.URI
	}
	return out
}

func candidatesOf(uris ...string) []candidate {
	out := make([]candidate, len(uris))
	for i, u := range uris {
		out[i] = candidate{URI: u}
	}
	return out
}

func TestMergeCandidates(t *testing.T) {
	for name, tc := range map[string]struct {
		first, second []candidate
		limit         int
		want          string
	}{
		"the newest, then the engaged ones they don't include": {candidatesOf("a", "b"), candidatesOf("c", "d"), 10, "a b c d"},
		"a post among both is kept once, in its first place":   {candidatesOf("a", "b"), candidatesOf("b", "c", "a"), 10, "a b c"},
		"nothing engaged": {candidatesOf("a", "b"), nil, 10, "a b"},
		"nothing new":     {nil, candidatesOf("c"), 10, "c"},
		"neither":         {nil, nil, 10, ""},
		"cut at the limit, keeping the newest first": {candidatesOf("a", "b"), candidatesOf("c", "d"), 3, "a b c"},
		"duplicates don't count toward the limit":    {candidatesOf("a", "b"), candidatesOf("a", "c", "d"), 3, "a b c"},
	} {
		if got := strings.Join(candidateURIs(mergeCandidates(tc.first, tc.second, tc.limit)), " "); got != tc.want {
			t.Errorf("%s: %q, want %q", name, got, tc.want)
		}
	}
}

func TestPoolQueryFollowsTheConfigAndTheRanking(t *testing.T) {
	cfg := PersonalConfigDefaults()
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

	q := cfg.PoolQuery(at, DefaultRanking)
	if !q.Since.Equal(at.Add(-24*time.Hour)) || q.Newest != 200 || q.Engaged != 150 || q.MinProb != 0.5 {
		t.Errorf("defaults: %+v", q)
	}
	if q.Weights != DefaultRanking.Weights {
		t.Errorf("the ranking's weights decide what a like or repost is worth: %+v", q.Weights)
	}
	// The engaged posts are chosen at the gravity of "popular", the setting that lets age count
	// least, so that every freshness setting finds what it wants among them.
	if q.Gravity != popularGravity {
		t.Errorf("gravity %v, want %v (the feed's own is %v)", q.Gravity, popularGravity, DefaultRanking.Gravity)
	}
	// A feed whose own ranking lets age count even less is chosen from at its gravity.
	r := DefaultRanking
	r.Gravity = 0.8
	if g := cfg.PoolQuery(at, r).Gravity; g != 0.8 {
		t.Errorf("gravity %v for a feed ranked at 0.8", g)
	}

	cfg.WindowHours, cfg.PerTopic, cfg.TopPerTopic, cfg.MinTopicProb = 6, 40, 25, 0.7
	q = cfg.PoolQuery(at, DefaultRanking)
	if !q.Since.Equal(at.Add(-6*time.Hour)) || q.Newest != 40 || q.Engaged != 25 || q.MinProb != 0.7 {
		t.Errorf("settings: %+v", q)
	}
}

func TestTopPerTopicSetting(t *testing.T) {
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

	if p, err := load("{}"); err != nil || p.TopPerTopic != 150 {
		t.Errorf("default: %+v, %v", p, err)
	}
	if p, err := load("{top_per_topic: 40}"); err != nil || p.TopPerTopic != 40 || p.PerTopic != 200 {
		t.Errorf("set: %+v, %v", p, err)
	}
	for _, bad := range []string{"{top_per_topic: 1001}", "{top_per_topic: -1}"} {
		if _, err := load(bad); err == nil || !strings.Contains(err.Error(), "top_per_topic") {
			t.Errorf("%s: %v, want an error naming top_per_topic", bad, err)
		}
	}
}

// The feed reads its pool with the feed's own settings: how many posts, from how far back, and
// the ranking that decides which engaged posts count.
func TestPersonalFeedAsksForItsPoolWithItsOwnSettings(t *testing.T) {
	src := &fakeSource{likes: aiLover(), pool: twoTopicPool(10)}
	newPersonalFor(t, src, func(c *PersonalConfig) { c.WindowHours, c.PerTopic, c.TopPerTopic = 12, 60, 45 })

	src.mu.Lock()
	defer src.mu.Unlock()
	if len(src.poolQueries) == 0 {
		t.Fatal("the pool was never read")
	}
	q := src.poolQueries[0]
	if !q.Since.Equal(now.Add(-12*time.Hour)) || q.Newest != 60 || q.Engaged != 45 || q.MinProb != 0.5 {
		t.Errorf("%+v", q)
	}
	if q.Gravity != popularGravity || q.Weights != DefaultRanking.Weights {
		t.Errorf("ranking: gravity %v, weights %+v", q.Gravity, q.Weights)
	}
}
