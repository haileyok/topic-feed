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
