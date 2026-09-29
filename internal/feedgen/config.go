// Package feedgen serves topic feeds to Bluesky: it picks recent classified posts from
// post_pipeline whose subtopic probability clears a feed's threshold, drops posts that
// were deleted, whose author is inactive, or that picked up moderation labels since they
// were processed, and answers the app.bsky.feed.* feed generator endpoints.
package feedgen

import (
	"fmt"
	"os"
	"regexp"
	"slices"
	"strings"
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
	// Paths are taxonomy subtopic paths (sports/american_football) or whole broad topics
	// (world_news). A post is a candidate when its probability for any of them is at least
	// MinProb: the subtopic probability for a path, the broad-topic probability for a
	// broad topic (which counts every subtopic, so posts split between siblings match).
	Paths []string `yaml:"paths"`
	// Exclude leaves out posts whose probability for a broad topic or subtopic is above
	// the given value, e.g. {adult_content: 0.2, art/commissions: 0.3}.
	Exclude map[string]float32 `yaml:"exclude"`
	MinProb float32            `yaml:"min_prob"`
	// AllowAdult admits posts the label policy marks adult_only. Posts it marks drop are
	// never shown.
	AllowAdult bool `yaml:"allow_adult"`
	// Ranking orders the feed; fields left out of the config keep DefaultRanking's values.
	Ranking Ranking `yaml:"ranking"`
	// AcceptsInteractions asks Bluesky to send interactions with the feed's posts
	// (default true). Takes effect after `feedgen publish`.
	AcceptsInteractions bool `yaml:"accepts_interactions"`
	// Tone filters and nudges posts by the model's tone probabilities.
	Tone ToneRules `yaml:"tone"`
	// MaxPosts caps the feed's candidates, newest first (0: FEEDGEN_MAX_POSTS). Busy
	// topics need more to reach back as far as quiet ones.
	MaxPosts int `yaml:"max_posts"`
}

// Tones are the model's tone labels; a post's tone probabilities sum to 1.
var Tones = []string{"informative", "humorous", "personal", "outraged", "supportive", "other"}

// ToneRules use the model's tone probabilities (0-1) in two ways:
//
//   - Max and Min are hard cutoffs: a post is left out when a tone's probability is above
//     its Max or below its Min. E.g. max {outraged: 0.5} drops angry posts;
//     min {humorous: 0.5} keeps only funny ones.
//   - Weights nudge the ranking: weight*probability is added to each post's prior, so
//     e.g. {outraged: -2, supportive: 1} sinks angry posts and lifts warm ones without
//     removing anything. A nudge matters most before a post has engagement.
type ToneRules struct {
	Max     map[string]float32 `yaml:"max"`
	Min     map[string]float32 `yaml:"min"`
	Weights map[string]float64 `yaml:"weights"`
}

// Allows reports whether a post with these tone probabilities passes the cutoffs.
func (t ToneRules) Allows(tone map[string]float32) bool {
	for name, max := range t.Max {
		if tone[name] > max {
			return false
		}
	}
	for name, min := range t.Min {
		if tone[name] < min {
			return false
		}
	}
	return true
}

// Nudge is the ranking adjustment for a post with these tone probabilities.
func (t ToneRules) Nudge(tone map[string]float32) float64 {
	n := 0.0
	for name, w := range t.Weights {
		n += w * float64(tone[name])
	}
	return n
}

func (t ToneRules) validate() error {
	known := func(name string) error {
		if !slices.Contains(Tones, name) {
			return fmt.Errorf("unknown tone %q (tones: %s)", name, strings.Join(Tones, ", "))
		}
		return nil
	}
	for _, m := range []map[string]float32{t.Max, t.Min} {
		for name, v := range m {
			if err := known(name); err != nil {
				return err
			}
			if v < 0 || v > 1 {
				return fmt.Errorf("tone %s: cutoffs are probabilities in [0, 1]", name)
			}
		}
	}
	for name := range t.Weights {
		if err := known(name); err != nil {
			return err
		}
	}
	return nil
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
	p := plain{Ranking: DefaultRanking, AcceptsInteractions: true}
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
				return fmt.Errorf("feed %q: %q is not a broad topic or subtopic path in the taxonomy", f.Rkey, p)
			}
		}
		for p, v := range f.Exclude {
			if !paths[p] {
				return fmt.Errorf("feed %q: exclude: %q is not a broad topic or subtopic path in the taxonomy", f.Rkey, p)
			}
			if v < 0 || v > 1 {
				return fmt.Errorf("feed %q: exclude %s: must be a probability in [0, 1]", f.Rkey, p)
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
		if f.MaxPosts < 0 || f.MaxPosts > 20000 {
			return fmt.Errorf("feed %q: max_posts must be 0-20000", f.Rkey)
		}
		if err := f.Tone.validate(); err != nil {
			return fmt.Errorf("feed %q: %w", f.Rkey, err)
		}
		if r.FreshEvery == 1 {
			return fmt.Errorf("feed %q: fresh_every 1 would make every slot fresh; use 0 for none, or 2 or more", f.Rkey)
		}
	}
	return nil
}

// TaxonomyPaths returns what a feed may name: every broad topic ID and every broad/sub
// subtopic path.
func TaxonomyPaths(tax *taxonomy.Taxonomy) map[string]bool {
	out := map[string]bool{}
	for _, b := range tax.Broad {
		out[b.ID] = true
		for _, s := range b.Subtopics {
			out[b.ID+"/"+s.ID] = true
		}
	}
	return out
}

// isBroad reports whether a feed path names a broad topic (no "/") rather than a subtopic.
func isBroad(p string) bool { return !strings.Contains(p, "/") }

// split separates a feed's paths into subtopic paths and broad topics. Each list has at
// least one entry ("" matches nothing), so queries never see an empty array.
func split(paths []string) (subs, broads []string) {
	for _, p := range paths {
		if isBroad(p) {
			broads = append(broads, p)
		} else {
			subs = append(subs, p)
		}
	}
	if len(subs) == 0 {
		subs = []string{""}
	}
	if len(broads) == 0 {
		broads = []string{""}
	}
	return subs, broads
}
