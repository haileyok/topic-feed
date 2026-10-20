package feedgen

import (
	"fmt"
	"maps"
	"slices"
	"strings"
)

// TopicRules are tone and signal rules for posts about one topic, in place of the feed's own
// for each score they name. A feed or a viewer's tuning keeps them by topic: a broad topic
// (us_politics) or a subtopic (us_politics/elections). A post counts as about its most likely
// subtopic, and the rules of that subtopic win over those of its broad topic, which win over
// the feed's own.
//
// They replace score by score: rules for a topic that name critical (with a cutoff or a boost)
// decide everything about critical for its posts, and leave every other score to the feed's
// own. A maximum of 1 lets every post through, so {max: {critical: 1}} lifts the feed's own
// cutoff on critical for the topic.
//
// MinProb (0: the feed's own) is how sure the model must be that a post is about the topic, in
// place of the feed's min_prob. In a feed of topic paths it is the threshold of the feed's path
// for the topic (a subtopic path takes its broad topic's when it has none of its own), so it can
// loosen or tighten the match; in a feed of every topic, and in a personal feed, posts whose most
// likely subtopic is in the topic need at least this probability for that subtopic.
type TopicRules struct {
	Tone    Rules   `yaml:"tone" json:"tone,omitzero"`
	Signals Rules   `yaml:"signals" json:"signals,omitzero"`
	MinProb float32 `yaml:"min_prob" json:"min_prob,omitempty"`
}

// maxTopicRules is how many topics a feed or a tuning can give rules of their own.
const maxTopicRules = 50

// IsZero reports whether the rules name no score.
func (t TopicRules) IsZero() bool { return t.Tone.IsZero() && t.Signals.IsZero() && t.MinProb == 0 }

// topicMinProb is the MinProb of the rules that decide it for posts whose most likely subtopic is
// topPath: the subtopic's, or else its broad topic's; 0 when neither has one.
func topicMinProb(topics map[string]TopicRules, topPath string) float32 {
	if v := topics[topPath].MinProb; v > 0 {
		return v
	}
	if b := broadOf(topPath); b != topPath {
		return topics[b].MinProb
	}
	return 0
}

// ruleKeyFor is which topic's MinProb decides it for posts whose most likely subtopic is topPath.
func ruleKeyFor(topics map[string]TopicRules, topPath string) string {
	if topics[topPath].MinProb > 0 {
		return topPath
	}
	return broadOf(topPath)
}

// pathMinProb is how sure the model must be of one of the feed's paths for a post to match it:
// the path's own rules, a subtopic path's broad topic's, or else the feed's min_prob.
func (f Feed) pathMinProb(path string) float32 {
	if v := topicMinProb(f.TopicRules, path); v > 0 {
		return v
	}
	return f.MinProb
}

// names is every score the rules name, with a cutoff or a boost.
func (r Rules) names() map[string]bool {
	out := map[string]bool{}
	for k := range r.Max {
		out[k] = true
	}
	for k := range r.Min {
		out[k] = true
	}
	for k := range r.Weights {
		out[k] = true
	}
	return out
}

// override is r with every score that o names decided by o alone.
func (r Rules) override(o Rules) Rules {
	names := o.names()
	if len(names) == 0 {
		return r
	}
	out := Rules{Max: map[string]float32{}, Min: map[string]float32{}, Weights: map[string]float64{}}
	for k, v := range r.Max {
		if !names[k] {
			out.Max[k] = v
		}
	}
	for k, v := range r.Min {
		if !names[k] {
			out.Min[k] = v
		}
	}
	for k, v := range r.Weights {
		if !names[k] {
			out.Weights[k] = v
		}
	}
	maps.Copy(out.Max, o.Max)
	maps.Copy(out.Min, o.Min)
	maps.Copy(out.Weights, o.Weights)
	return out
}

// broadOf is the broad topic of a subtopic path, or the path itself when it has no subtopic.
func broadOf(path string) string {
	if i := strings.IndexByte(path, '/'); i >= 0 {
		return path[:i]
	}
	return path
}

// rulesFor is global with the rules for topPath's broad topic, then for topPath itself, applied.
func rulesFor(global TopicRules, topics map[string]TopicRules, topPath string) TopicRules {
	if len(topics) == 0 {
		return global
	}
	out := global
	apply := func(key string) {
		if o, ok := topics[key]; ok {
			out.Tone = out.Tone.override(o.Tone)
			out.Signals = out.Signals.override(o.Signals)
		}
	}
	if b := broadOf(topPath); b != topPath {
		apply(b)
	}
	apply(topPath)
	return out
}

// ruleSource is which of topics decides a score for posts about topPath ("" for the global rules):
// kind is "tone" or "signal".
func ruleSource(topics map[string]TopicRules, topPath, kind, name string) string {
	for _, key := range []string{topPath, broadOf(topPath)} {
		o, ok := topics[key]
		if !ok {
			continue
		}
		r := o.Tone
		if kind == "signal" {
			r = o.Signals
		}
		if r.names()[name] {
			return key
		}
	}
	return ""
}

// RulesFor is the feed's tone and signal rules for a post whose most likely subtopic is topPath.
func (f Feed) RulesFor(topPath string) TopicRules {
	return rulesFor(TopicRules{Tone: f.Tone, Signals: f.Signals}, f.TopicRules, topPath)
}

// validateTopicRules checks each topic is in the taxonomy (known, when known is not nil) and its
// rules with check, which is given the kind ("tone" or "signal"), the rules and the names.
func validateTopicRules(topics map[string]TopicRules, known map[string]bool, check func(kind string, r Rules, names []string) error) error {
	if len(topics) > maxTopicRules {
		return fmt.Errorf("at most %d topics can have rules of their own", maxTopicRules)
	}
	for _, key := range slices.Sorted(maps.Keys(topics)) {
		if known != nil && !known[key] {
			return fmt.Errorf("topic rules: %q is not a broad topic or subtopic path in the taxonomy", key)
		}
		o := topics[key]
		if !(o.MinProb >= 0 && o.MinProb <= 1) { // also rejects NaN
			return fmt.Errorf("topic rules for %s: min_prob must be between 0 and 1 (0 for the feed's own)", key)
		}
		if err := check("tone", o.Tone, Tones); err != nil {
			return fmt.Errorf("topic rules for %s: %w", key, err)
		}
		if err := check("signal", o.Signals, Signals); err != nil {
			return fmt.Errorf("topic rules for %s: %w", key, err)
		}
	}
	return nil
}
