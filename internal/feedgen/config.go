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
	"time"
	"unicode/utf8"

	"gopkg.in/yaml.v3"

	"github.com/haileyok/topic-feed/internal/taxonomy"
)

// Feed is one feed from config/feeds.yaml.
type Feed struct {
	// Owner is the DID of the account whose repo holds the feed's record, when that isn't the
	// owner of the service: the feeds people make on the web. Empty means the service owner's,
	// which is every feed in the config file.
	Owner string `yaml:"-" json:"-"`
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
	// Tone and Signals filter and nudge posts by the model's tone probabilities and
	// signal scores (see Rules).
	Tone    Rules `yaml:"tone"`
	Signals Rules `yaml:"signals"`
	// MaxPosts caps the feed's candidates, newest first (0: FEEDGEN_MAX_POSTS). Busy
	// topics need more to reach back as far as quiet ones.
	MaxPosts int `yaml:"max_posts"`
	// MaxAgeMinutes leaves out posts older than this, however popular (0: the service's window,
	// FEEDGEN_WINDOW_HOURS). It can only shorten that window.
	MaxAgeMinutes int `yaml:"max_age_minutes"`
	// Personal makes this a feed built for each viewer from the posts they liked, rather
	// than from topic paths: paths, min_prob, exclude, tone and signals don't apply. Use
	// `personal: {}` for the defaults. See PersonalConfig.
	Personal *PersonalConfig `yaml:"personal"`
}

// PersonalConfig tunes a personal feed. A viewer's interests are the subtopics of the posts
// they liked and reposted, newer likes counting more; the feed fills its slots in
// proportion to those interests, taking the best-ranked recent posts of each subtopic, and
// leaves out what the viewer has already seen.
type PersonalConfig struct {
	// LookbackDays is how far back likes and reposts count (default 30; the likes table
	// itself keeps 14 days).
	LookbackDays int `yaml:"lookback_days"`
	// HalfLifeDays: a like's weight halves every this many days (default 7).
	HalfLifeDays float64 `yaml:"half_life_days"`
	// MinLikes is how many liked, classified posts a viewer needs for their own interests
	// to be used; with fewer they get a mix of every topic (default 5).
	MinLikes int `yaml:"min_likes"`
	// Topics is how many interests are used, strongest first (default 20).
	Topics int `yaml:"topics"`
	// WindowHours: only posts at most this old are shown (default 24).
	WindowHours int `yaml:"window_hours"`
	// PerTopic is how many of the newest posts are kept for each subtopic (default 200).
	PerTopic int `yaml:"per_topic"`
	// TopPerTopic is how many more are kept for each subtopic: the posts with the most
	// engagement for their age from anywhere in the window (default 150). A busy subtopic gets
	// thousands of posts an hour, so its newest PerTopic cover only minutes: without these the
	// feed would show nothing older than that, however well liked.
	TopPerTopic int `yaml:"top_per_topic"`
	// MinTopicProb: a post belongs to its most likely subtopic when the model gives it at
	// least this probability (default 0.5).
	MinTopicProb float32 `yaml:"min_topic_prob"`
	// MaxServes is how many times a post may be shown before it counts as seen even though
	// Bluesky never reported a view (default 2; 1 never repeats a post, but loses the posts
	// that were sent and not scrolled to).
	MaxServes int `yaml:"max_serves"`
	// ListSize is how many posts a viewer's feed holds at a time (default 300).
	ListSize int `yaml:"list_size"`
	// AuthorGap keeps an author's posts at least this many slots apart (default 10; 0: no limit).
	AuthorGap *int `yaml:"author_gap"`
	// MinEngagement is how much engagement a post needs before the feed shows it: its likes,
	// reposts, replies, and quotes counted at the feed's ranking weights (default 5; 0: none).
	// Posts below it are left out, even when nothing else is left to show: a subtopic that has
	// run out of posts people have reacted to gives its slots to the others, and when they all
	// have, the feed ends rather than filling up with posts nobody has reacted to yet.
	MinEngagement *float64 `yaml:"min_engagement"`
}

// PersonalConfigDefaults is the configuration of `personal: {}`.
func PersonalConfigDefaults() PersonalConfig {
	var c PersonalConfig
	c.applyDefaults()
	return c
}

func (c *PersonalConfig) applyDefaults() {
	set := func(v *int, def int) {
		if *v == 0 {
			*v = def
		}
	}
	set(&c.LookbackDays, 30)
	set(&c.MinLikes, 5)
	set(&c.Topics, 20)
	set(&c.WindowHours, 24)
	set(&c.PerTopic, 200)
	set(&c.TopPerTopic, 150)
	set(&c.MaxServes, 2)
	set(&c.ListSize, 300)
	if c.HalfLifeDays == 0 {
		c.HalfLifeDays = 7
	}
	if c.MinTopicProb == 0 {
		c.MinTopicProb = 0.5
	}
	if c.AuthorGap == nil {
		gap := 10
		c.AuthorGap = &gap
	}
	if c.MinEngagement == nil {
		minEngagement := DefaultMinEngagement
		c.MinEngagement = &minEngagement
	}
}

// DefaultMinEngagement is what `min_engagement` is when a personal feed doesn't set it.
const DefaultMinEngagement = 5.0

// MaxMinEngagement is the most a feed or a viewer can ask of a post.
const MaxMinEngagement = 200.0

func (c PersonalConfig) validate() error {
	switch {
	case c.LookbackDays < 1 || c.LookbackDays > 365:
		return fmt.Errorf("lookback_days must be 1-365")
	case c.HalfLifeDays <= 0 || c.HalfLifeDays > 365:
		return fmt.Errorf("half_life_days must be above 0 and at most 365")
	case c.MinLikes < 1:
		return fmt.Errorf("min_likes must be at least 1")
	case c.Topics < 1 || c.Topics > 100:
		return fmt.Errorf("topics must be 1-100")
	case c.WindowHours < 1 || c.WindowHours > 168:
		return fmt.Errorf("window_hours must be 1-168")
	case c.PerTopic < 1 || c.PerTopic > 1000:
		return fmt.Errorf("per_topic must be 1-1000")
	case c.TopPerTopic < 1 || c.TopPerTopic > 1000:
		return fmt.Errorf("top_per_topic must be 1-1000")
	case c.MinTopicProb <= 0 || c.MinTopicProb > 1:
		return fmt.Errorf("min_topic_prob must be in (0, 1]")
	case c.MaxServes < 1 || c.MaxServes > 20:
		return fmt.Errorf("max_serves must be 1-20")
	case c.ListSize < 1 || c.ListSize > 2000:
		return fmt.Errorf("list_size must be 1-2000")
	case c.AuthorGap != nil && *c.AuthorGap < 0:
		return fmt.Errorf("author_gap can't be negative")
	case c.MinEngagement != nil && !(*c.MinEngagement >= 0 && *c.MinEngagement <= MaxMinEngagement):
		return fmt.Errorf("min_engagement must be 0-%g", MaxMinEngagement)
	}
	return nil
}

// Tones are the model's tone labels; a post's tone probabilities sum to 1.
var Tones = []string{"informative", "humorous", "personal", "outraged", "supportive", "other"}

// Signals are the model's independent 0-1 scores. Since v4 (2026-09-30): sentiment
// (0 very negative .. 1 very positive), critical (negative about or mocking what the post
// is about), and the kinds of promotion: ad, engagement_bait, spam, self_promo. Since v5
// (the model that looks at pictures): meme, a captioned, edited or AI-generated joke image or
// a reaction image. It is only stored for posts whose pictures the model saw, so it is
// missing on text-only posts, and older models don't produce the newer scores either: a
// cutoff on a missing score treats it as 0.
var Signals = []string{"substance", "news", "promo", "general_interest",
	"sentiment", "critical", "ad", "engagement_bait", "spam", "self_promo", "meme"}

// Rules use one of the model's score sets (tone or signals, each 0-1) in two ways:
//
//   - Max and Min are hard cutoffs: a post is left out when a score is above its Max or
//     below its Min. E.g. tone max {outraged: 0.5} drops angry posts; tone min
//     {humorous: 0.5} keeps only funny ones; signals min {substance: 0.8} keeps meaty ones.
//   - Weights nudge the ranking: weight*score is added to each post's prior, so e.g. tone
//     {outraged: -2, supportive: 1} sinks angry posts and lifts warm ones without removing
//     anything. A nudge matters most before a post has engagement.
type Rules struct {
	Max     map[string]float32 `yaml:"max" json:"max,omitempty"`
	Min     map[string]float32 `yaml:"min" json:"min,omitempty"`
	Weights map[string]float64 `yaml:"weights" json:"weights,omitempty"`
}

// Allows reports whether a post with these scores passes the cutoffs.
func (t Rules) Allows(scores map[string]float32) bool {
	for name, max := range t.Max {
		if scores[name] > max {
			return false
		}
	}
	for name, min := range t.Min {
		if scores[name] < min {
			return false
		}
	}
	return true
}

// Nudge is the ranking adjustment for a post with these scores.
func (t Rules) Nudge(scores map[string]float32) float64 {
	n := 0.0
	for name, w := range t.Weights {
		n += w * float64(scores[name])
	}
	return n
}

// validate checks names against the score set (kind is "tone" or "signal") and ranges.
func (t Rules) validate(kind string, names []string) error {
	known := func(name string) error {
		if !slices.Contains(names, name) {
			return fmt.Errorf("unknown %s %q (%ss: %s)", kind, name, kind, strings.Join(names, ", "))
		}
		return nil
	}
	for _, m := range []map[string]float32{t.Max, t.Min} {
		for name, v := range m {
			if err := known(name); err != nil {
				return err
			}
			if v < 0 || v > 1 {
				return fmt.Errorf("%s %s: cutoffs are in [0, 1]", kind, name)
			}
		}
	}
	for name, w := range t.Weights {
		if err := known(name); err != nil {
			return err
		}
		if w < -10 || w > 10 {
			return fmt.Errorf("%s %s: weights are in [-10, 10]", kind, name)
		}
	}
	return nil
}

// AnyTopic in a feed's paths matches every classified post; the feed then selects by its
// exclusions, tone, and signal rules alone.
const AnyTopic = "*"

func (f Feed) anyTopic() bool { return slices.Contains(f.Paths, AnyTopic) }

// Ranking controls a feed's order. Each post scores
//
//	(prior + engagement^engagement_power) / (age_hours + 2)^gravity
//
// where engagement = like*likes + repost*reposts + reply*replies + quote*quotes, and prior =
// max(0.1, 1 + substance + general_interest - promo_penalty*promo), from the model's signals,
// so posts without engagement yet are still ordered sensibly. Every FreshEvery-th slot goes to
// the newest post not already placed, whatever its score, and an author's posts are kept at
// least AuthorGap slots apart.
//
// EngagementPower below 1 makes big numbers count less: at 0.5 a post with 4,000 likes counts
// twice as much as one with 1,000, not four times, so a post that went viral hours ago can't
// outlast its age. 0 (left out) is 1: engagement counts in full.
type Ranking struct {
	Weights         Weights `yaml:"weights" json:"weights"`
	Gravity         float64 `yaml:"gravity" json:"gravity"`                             // higher: older posts sink faster
	FreshEvery      int     `yaml:"fresh_every" json:"fresh_every"`                     // 0: no fresh slots
	AuthorGap       int     `yaml:"author_gap" json:"author_gap"`                       // 0: no limit
	PromoPenalty    float64 `yaml:"promo_penalty" json:"promo_penalty"`                 // 0: promotional posts aren't penalized
	EngagementPower float64 `yaml:"engagement_power" json:"engagement_power,omitempty"` // 0 or 1: in full
}

// MinEngagementPower is the least EngagementPower can be: lower, posts nobody has reacted to
// crowd out the ones people have.
const MinEngagementPower = 0.2

// power is the EngagementPower in effect.
func (r Ranking) power() float64 {
	if r.EngagementPower == 0 {
		return 1
	}
	return r.EngagementPower
}

func (r Ranking) validate() error {
	if r.Gravity < 0 || r.FreshEvery < 0 || r.AuthorGap < 0 || r.PromoPenalty < 0 ||
		r.Weights.Like < 0 || r.Weights.Repost < 0 || r.Weights.Reply < 0 || r.Weights.Quote < 0 {
		return fmt.Errorf("ranking values can't be negative")
	}
	if p := r.EngagementPower; p != 0 && !(p >= MinEngagementPower && p <= 1) {
		return fmt.Errorf("engagement_power must be between %g and 1 (0 for 1)", MinEngagementPower)
	}
	return nil
}

// Weights are engagement units per like, repost, reply, and quote.
type Weights struct {
	Like   float64 `yaml:"like" json:"like"`
	Repost float64 `yaml:"repost" json:"repost"`
	Reply  float64 `yaml:"reply" json:"reply"`
	Quote  float64 `yaml:"quote" json:"quote"`
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
	if p.Personal != nil {
		p.Personal.applyDefaults()
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
		if err := f.validate(paths); err != nil {
			return err
		}
		if seen[f.Rkey] {
			return fmt.Errorf("feed %q: duplicate rkey", f.Rkey)
		}
		seen[f.Rkey] = true
	}
	return nil
}

// validate checks one feed against the lexicon limits and the known subtopic paths. The errors
// name the rkey and are worded for the person who wrote the feed.
func (f Feed) validate(paths map[string]bool) error {
	if !rkeyPattern.MatchString(f.Rkey) {
		return fmt.Errorf("feed %q: rkey must be 1-15 lowercase letters, digits, or dashes", f.Rkey)
	}
	if n := utf8.RuneCountInString(f.DisplayName); n == 0 || n > maxDisplayName {
		return fmt.Errorf("feed %q: display_name must be 1-%d characters", f.Rkey, maxDisplayName)
	}
	if utf8.RuneCountInString(f.Description) > maxDescription {
		return fmt.Errorf("feed %q: description is over %d characters", f.Rkey, maxDescription)
	}
	if f.Personal != nil {
		// A personal feed has no topic paths: the viewer's likes choose them.
		if len(f.Paths) > 0 || f.MinProb != 0 || len(f.Exclude) > 0 {
			return fmt.Errorf("feed %q: a personal feed takes its topics from the viewer's likes; remove paths, min_prob and exclude", f.Rkey)
		}
		if f.MaxAgeMinutes != 0 {
			return fmt.Errorf("feed %q: a personal feed reaches back personal.window_hours; remove max_age_minutes", f.Rkey)
		}
		if err := f.Personal.validate(); err != nil {
			return fmt.Errorf("feed %q: personal: %w", f.Rkey, err)
		}
		if err := f.Ranking.validate(); err != nil {
			return fmt.Errorf("feed %q: %w", f.Rkey, err)
		}
		return nil
	}
	if len(f.Paths) == 0 {
		return fmt.Errorf("feed %q: no paths", f.Rkey)
	}
	if f.anyTopic() && len(f.Paths) > 1 {
		return fmt.Errorf("feed %q: %q (any topic) can't be combined with other paths", f.Rkey, AnyTopic)
	}
	for _, p := range f.Paths {
		if p != AnyTopic && !paths[p] {
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
	if err := r.validate(); err != nil {
		return fmt.Errorf("feed %q: %w", f.Rkey, err)
	}
	if f.MaxPosts < 0 || f.MaxPosts > 20000 {
		return fmt.Errorf("feed %q: max_posts must be 0-20000", f.Rkey)
	}
	if f.MaxAgeMinutes != 0 && (f.MaxAgeMinutes < MinMaxAgeMinutes || f.MaxAgeMinutes > MaxMaxAgeMinutes) {
		return fmt.Errorf("feed %q: max_age_minutes must be %d-%d (0 for the service's window)", f.Rkey, MinMaxAgeMinutes, MaxMaxAgeMinutes)
	}
	if err := f.Tone.validate("tone", Tones); err != nil {
		return fmt.Errorf("feed %q: %w", f.Rkey, err)
	}
	if err := f.Signals.validate("signal", Signals); err != nil {
		return fmt.Errorf("feed %q: %w", f.Rkey, err)
	}
	if r.FreshEvery == 1 {
		return fmt.Errorf("feed %q: fresh_every 1 would make every slot fresh; use 0 for none, or 2 or more", f.Rkey)
	}
	return nil
}

// The range of a feed's max_age_minutes: half an hour to a day.
const (
	MinMaxAgeMinutes = 30
	MaxMaxAgeMinutes = 24 * 60
)

// Since is when the feed's posts start, given the service's window: the window, or the feed's
// max age where that is shorter.
func (f Feed) Since(now time.Time, window time.Duration) time.Time {
	return now.Add(-f.Window(window))
}

// Window is how far back the feed reaches, given the service's window.
func (f Feed) Window(window time.Duration) time.Duration {
	if f.MaxAgeMinutes > 0 {
		return min(window, time.Duration(f.MaxAgeMinutes)*time.Minute)
	}
	return window
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
