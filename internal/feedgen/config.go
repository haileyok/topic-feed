// Package feedgen serves topic feeds to Bluesky: it picks recent classified posts from
// post_pipeline whose subtopic probability clears a feed's threshold, drops posts that
// were deleted, whose author is inactive, or that picked up moderation labels since they
// were processed, and answers the app.bsky.feed.* feed generator endpoints.
package feedgen

import (
	"fmt"
	"os"
	"regexp"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/haileyok/topic-feed/internal/taxonomy"
)

// Feed is one feed from config/feeds.yaml.
type Feed struct {
	// Rkey is the record key of the feed's app.bsky.feed.generator record, and the last
	// part of its at:// URI.
	Rkey        string `yaml:"rkey"`
	DisplayName string `yaml:"display_name"`
	Description string `yaml:"description"`
	// Paths are taxonomy subtopic paths, e.g. sports/american_football. A post is a
	// candidate when its probability for any of them is at least MinProb.
	Paths   []string `yaml:"paths"`
	MinProb float32  `yaml:"min_prob"`
	// AllowAdult admits posts the label policy marks adult_only. Posts it marks drop are
	// never shown.
	AllowAdult bool `yaml:"allow_adult"`
	// Ranking orders the feed; fields left out of the config keep DefaultRanking's values.
	Ranking Ranking `yaml:"ranking"`
}

// Ranking controls a feed's order. Each post scores
//
//	(prior + like*likes + repost*reposts + reply*replies + quote*quotes) / (age_hours + 2)^gravity
//
// where prior = max(0.1, 1 + substance + general_interest - promo_penalty*promo), from the
// model's signals, so posts without engagement yet are still ordered sensibly. Every
// FreshEvery-th slot goes to the newest post not already placed, whatever its score, and
// an author's posts are kept at least AuthorGap slots apart.
type Ranking struct {
	Weights      Weights `yaml:"weights"`
	Gravity      float64 `yaml:"gravity"`       // higher: older posts sink faster
	FreshEvery   int     `yaml:"fresh_every"`   // 0: no fresh slots
	AuthorGap    int     `yaml:"author_gap"`    // 0: no limit
	PromoPenalty float64 `yaml:"promo_penalty"` // 0: promotional posts aren't penalized
}

// Weights are engagement units per like, repost, reply, and quote.
type Weights struct {
	Like   float64 `yaml:"like"`
	Repost float64 `yaml:"repost"`
	Reply  float64 `yaml:"reply"`
	Quote  float64 `yaml:"quote"`
}

// DefaultRanking applies to every feed unless its config overrides a field.
var DefaultRanking = Ranking{
	Weights:      Weights{Like: 1, Repost: 2, Reply: 2, Quote: 3},
	Gravity:      1.8,
	FreshEvery:   4,
	AuthorGap:    10,
	PromoPenalty: 1,
}

// UnmarshalYAML starts every feed from DefaultRanking, so the config only lists changes.
func (f *Feed) UnmarshalYAML(n *yaml.Node) error {
	type plain Feed
	p := plain{Ranking: DefaultRanking}
	if err := n.Decode(&p); err != nil {
		return err
	}
	*f = Feed(p)
	return nil
}

// Config is config/feeds.yaml.
type Config struct {
	Feeds []Feed `yaml:"feeds"`
}

// Record keys for feeds: Bluesky accepts at most 15 characters.
var rkeyPattern = regexp.MustCompile(`^[a-z0-9][a-z0-9-]{0,14}$`)

// Lexicon limits of app.bsky.feed.generator (graphemes; runes are a close upper bound).
const (
	maxDisplayName = 24
	maxDescription = 300
)

// LoadConfig reads and checks the feed config. Every path must exist in the taxonomy.
func LoadConfig(path string, tax *taxonomy.Taxonomy) (*Config, error) {
	b, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	var c Config
	if err := yaml.Unmarshal(b, &c); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	if err := c.Validate(TaxonomyPaths(tax)); err != nil {
		return nil, fmt.Errorf("%s: %w", path, err)
	}
	return &c, nil
}

// Validate checks every feed against the lexicon limits and the known subtopic paths.
func (c *Config) Validate(paths map[string]bool) error {
	if len(c.Feeds) == 0 {
		return fmt.Errorf("no feeds")
	}
	seen := map[string]bool{}
	for _, f := range c.Feeds {
		if !rkeyPattern.MatchString(f.Rkey) {
			return fmt.Errorf("feed %q: rkey must be 1-15 lowercase letters, digits, or dashes", f.Rkey)
		}
		if seen[f.Rkey] {
			return fmt.Errorf("feed %q: duplicate rkey", f.Rkey)
		}
		seen[f.Rkey] = true
		if n := utf8.RuneCountInString(f.DisplayName); n == 0 || n > maxDisplayName {
			return fmt.Errorf("feed %q: display_name must be 1-%d characters", f.Rkey, maxDisplayName)
		}
		if utf8.RuneCountInString(f.Description) > maxDescription {
			return fmt.Errorf("feed %q: description is over %d characters", f.Rkey, maxDescription)
		}
		if len(f.Paths) == 0 {
			return fmt.Errorf("feed %q: no paths", f.Rkey)
		}
		for _, p := range f.Paths {
			if !paths[p] {
				return fmt.Errorf("feed %q: %q is not a subtopic path in the taxonomy", f.Rkey, p)
			}
		}
		if f.MinProb <= 0 || f.MinProb > 1 {
			return fmt.Errorf("feed %q: min_prob must be in (0, 1]", f.Rkey)
		}
		r := f.Ranking
		if r.Gravity < 0 || r.FreshEvery < 0 || r.AuthorGap < 0 || r.PromoPenalty < 0 ||
			r.Weights.Like < 0 || r.Weights.Repost < 0 || r.Weights.Reply < 0 || r.Weights.Quote < 0 {
			return fmt.Errorf("feed %q: ranking values can't be negative", f.Rkey)
		}
		if r.FreshEvery == 1 {
			return fmt.Errorf("feed %q: fresh_every 1 would make every slot fresh; use 0 for none, or 2 or more", f.Rkey)
		}
	}
	return nil
}

// TaxonomyPaths returns the model's path labels: broad/sub for every subtopic, and the
// bare broad ID for topics without subtopics (e.g. unclear).
func TaxonomyPaths(tax *taxonomy.Taxonomy) map[string]bool {
	out := map[string]bool{}
	for _, b := range tax.Broad {
		if len(b.Subtopics) == 0 {
			out[b.ID] = true
		}
		for _, s := range b.Subtopics {
			out[b.ID+"/"+s.ID] = true
		}
	}
	return out
}
