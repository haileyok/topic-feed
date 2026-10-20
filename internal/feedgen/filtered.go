package feedgen

import (
	"fmt"
	"maps"
	"slices"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// FilteredConfig makes a feed show another feed's posts, less the ones its filters leave out
// (`filtered:` in feeds.yaml). The posts keep the other feed's order: nothing is added, ranked or
// boosted. Each viewer's requests to the other feed are made with a token their own server signs,
// so the feed must be one they have signed in for on the page at /filtered. Viewers who haven't,
// or whose sign-in has run out, get only the post that says so (FEEDGEN_FILTER_SIGNIN_POST).
//
// The feed's exclude, tone, signals and topic_rules are the filters viewers start from: the
// cutoffs only (max and min), since nothing is ranked. A viewer can choose their own on that page.
type FilteredConfig struct {
	// Source is the at:// URI of the other feed's app.bsky.feed.generator record.
	Source string `yaml:"source"`
	// DropUnscored leaves out the posts the model never scored (posts not in English, and
	// replies), which no filter can judge. Off: they are kept.
	DropUnscored bool `yaml:"drop_unscored"`
}

// Filters are what a filtered feed leaves out of its source's posts: a viewer's own, or the
// feed's (Feed.Filters).
type Filters struct {
	// Exclude leaves out posts whose probability for a broad topic or subtopic is above the value.
	Exclude map[string]float32 `json:"exclude,omitempty"`
	// Tone and Signals leave out posts above a score's max or below its min. Weights are refused.
	Tone    Rules `json:"tone"`
	Signals Rules `json:"signals"`
	// TopicRules replace Tone and Signals, score by score, for posts whose most likely subtopic is
	// in a topic, as in a topic feed (a subtopic's win over its broad topic's).
	TopicRules map[string]TopicRules `json:"topic_rules,omitempty"`
	// DropUnscored leaves out the posts the model never scored.
	DropUnscored bool `json:"drop_unscored,omitempty"`
}

// maxFilterExclude bounds how many topics one set of filters leaves out.
const maxFilterExclude = 60

// Filters are the feed's own filters, which viewers who haven't chosen theirs get.
func (f Feed) Filters() Filters {
	fl := Filters{Exclude: f.Exclude, Tone: f.Tone, Signals: f.Signals, TopicRules: f.TopicRules}
	if f.Filtered != nil {
		fl.DropUnscored = f.Filtered.DropUnscored
	}
	return fl
}

// validate checks the filters against the known topic paths (nil: any) and the ranges.
func (fl Filters) validate(paths map[string]bool) error {
	if len(fl.Exclude) > maxFilterExclude {
		return fmt.Errorf("at most %d topics to leave out", maxFilterExclude)
	}
	for _, p := range slices.Sorted(maps.Keys(fl.Exclude)) {
		if paths != nil && !paths[p] {
			return fmt.Errorf("exclude: %q is not a broad topic or subtopic path in the taxonomy", p)
		}
		if v := fl.Exclude[p]; !(v >= 0 && v <= 1) {
			return fmt.Errorf("exclude %s: must be a probability in [0, 1]", p)
		}
	}
	check := func(kind string, r Rules, names []string) error {
		if len(r.Weights) > 0 {
			return fmt.Errorf("%s weights: a filtered feed only leaves posts out, it doesn't rank them (use max and min)", kind)
		}
		return r.validate(kind, names)
	}
	if err := check("tone", fl.Tone, Tones); err != nil {
		return err
	}
	if err := check("signal", fl.Signals, Signals); err != nil {
		return err
	}
	if err := validateTopicRules(fl.TopicRules, paths, check); err != nil {
		return err
	}
	for _, key := range slices.Sorted(maps.Keys(fl.TopicRules)) {
		if fl.TopicRules[key].MinProb != 0 {
			return fmt.Errorf("topic rules for %s: min_prob doesn't apply to a filtered feed", key)
		}
	}
	return nil
}

// ScoredPost is what the model made of a post from a source feed. Scored is false when it never
// scored it.
type ScoredPost struct {
	URI        string
	Scored     bool
	PathProbs  map[string]float32
	BroadProbs map[string]float32
	Tone       map[string]float32
	Signals    map[string]float32
	TopPath    string
}

// Keeps reports whether the filters let the post through. A post the model never scored is kept
// unless DropUnscored. A topic's
// probability is the subtopic's for a subtopic path and the broad topic's for a broad topic, as in a
// topic feed's exclude; a score the model didn't give is 0.
func (fl Filters) Keeps(p ScoredPost) bool { return fl.Why(p) == nil }

// LeftOutReason is which filter left a post out. Kind is "topic", "tone", "signal" or "unscored";
// Name the topic or score; Value the post's probability or score, and Cutoff the
// filter's (Bound says whether it was a maximum or a minimum). Rule is the topic whose own rules
// decided a tone or signal cutoff, or "".
type LeftOutReason struct {
	Kind   string  `json:"kind"`
	Name   string  `json:"name,omitempty"`
	Value  float32 `json:"value,omitempty"`
	Cutoff float32 `json:"cutoff,omitempty"`
	Bound  string  `json:"bound,omitempty"`
	Rule   string  `json:"rule,omitempty"`
}

// Why is the first filter that leaves the post out (unscored, then topics, then cutoffs, each in
// name order), or nil when it is kept.
func (fl Filters) Why(p ScoredPost) *LeftOutReason {
	if !p.Scored {
		if fl.DropUnscored {
			return &LeftOutReason{Kind: "unscored"}
		}
		return nil
	}
	for _, path := range slices.Sorted(maps.Keys(fl.Exclude)) {
		prob := p.PathProbs[path]
		if isBroad(path) {
			prob = p.BroadProbs[path]
		}
		if max := fl.Exclude[path]; prob > max {
			return &LeftOutReason{Kind: "topic", Name: path, Value: prob, Cutoff: max, Bound: "max"}
		}
	}
	r := rulesFor(TopicRules{Tone: fl.Tone, Signals: fl.Signals}, fl.TopicRules, p.TopPath)
	for _, set := range []struct {
		kind   string
		rules  Rules
		scores map[string]float32
	}{{"tone", r.Tone, p.Tone}, {"signal", r.Signals, p.Signals}} {
		for _, name := range slices.Sorted(maps.Keys(set.rules.Max)) {
			if v, max := set.scores[name], set.rules.Max[name]; v > max {
				return &LeftOutReason{Kind: set.kind, Name: name, Value: v, Cutoff: max, Bound: "max", Rule: ruleSource(fl.TopicRules, p.TopPath, set.kind, name)}
			}
		}
		for _, name := range slices.Sorted(maps.Keys(set.rules.Min)) {
			if v, min := set.scores[name], set.rules.Min[name]; v < min {
				return &LeftOutReason{Kind: set.kind, Name: name, Value: v, Cutoff: min, Bound: "min", Rule: ruleSource(fl.TopicRules, p.TopPath, set.kind, name)}
			}
		}
	}
	return nil
}

// validateFiltered checks a filtered feed: a generator record to show, and filters that only
// leave posts out.
func (f Feed) validateFiltered(paths map[string]bool) error {
	u, err := syntax.ParseATURI(f.Filtered.Source)
	if err != nil || u.Collection().String() != generatorCollection || u.RecordKey().String() == "" {
		return fmt.Errorf("feed %q: filtered.source must be the at:// URI of a feed (at://did:.../app.bsky.feed.generator/...), got %q", f.Rkey, f.Filtered.Source)
	}
	if _, err := u.Authority().AsDID(); err != nil {
		return fmt.Errorf("feed %q: filtered.source must name the feed's account by its DID, not a handle", f.Rkey)
	}
	switch {
	case f.Personal != nil:
		return fmt.Errorf("feed %q: a feed can't be both personal and filtered", f.Rkey)
	case len(f.Paths) > 0 || f.MinProb != 0:
		return fmt.Errorf("feed %q: a filtered feed shows another feed's posts; remove paths and min_prob", f.Rkey)
	case f.MaxPosts != 0 || f.MaxAgeMinutes != 0:
		return fmt.Errorf("feed %q: a filtered feed shows another feed's posts; remove max_posts and max_age_minutes", f.Rkey)
	case f.AllowAdult:
		return fmt.Errorf("feed %q: allow_adult doesn't apply to a filtered feed", f.Rkey)
	}
	if err := f.Filters().validate(paths); err != nil {
		return fmt.Errorf("feed %q: %w", f.Rkey, err)
	}
	return nil
}
