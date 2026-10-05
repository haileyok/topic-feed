package feedgen

import (
	"encoding/json"
	"strings"
	"testing"
)

// The feed's own rules, a broad topic's, and a subtopic's: each replaces, score by score, what
// came before it for its posts.
func topicRulesFeed() Feed {
	return Feed{Rkey: "f", DisplayName: "F", Paths: []string{"us_politics", "technology"}, MinProb: 0.5, Ranking: DefaultRanking,
		Tone:    Rules{Max: map[string]float32{"outraged": 0.3}, Weights: map[string]float64{"humorous": 1}},
		Signals: Rules{Max: map[string]float32{"critical": 0.5}, Min: map[string]float32{"substance": 0.4}},
		TopicRules: map[string]TopicRules{
			"us_politics":           {Signals: Rules{Max: map[string]float32{"critical": 1}}},
			"us_politics/elections": {Signals: Rules{Max: map[string]float32{"critical": 0.2}}, Tone: Rules{Weights: map[string]float64{"humorous": -1}}},
		}}
}

func TestRulesForATopic(t *testing.T) {
	f := topicRulesFeed()
	own := f.RulesFor("technology/ai")
	if own.Signals.Max["critical"] != 0.5 || own.Signals.Min["substance"] != 0.4 || own.Tone.Max["outraged"] != 0.3 {
		t.Errorf("a topic without rules has the feed's own: %+v", own)
	}
	broad := f.RulesFor("us_politics/congress")
	if broad.Signals.Max["critical"] != 1 {
		t.Errorf("the broad topic's rule: %+v", broad.Signals)
	}
	if broad.Signals.Min["substance"] != 0.4 || broad.Tone.Max["outraged"] != 0.3 || broad.Tone.Weights["humorous"] != 1 {
		t.Errorf("scores the topic's rules don't name stay the feed's: %+v", broad)
	}
	sub := f.RulesFor("us_politics/elections")
	if sub.Signals.Max["critical"] != 0.2 || sub.Tone.Weights["humorous"] != -1 || sub.Tone.Max["outraged"] != 0.3 {
		t.Errorf("the subtopic wins over its broad topic: %+v", sub)
	}
	// Replacing a score replaces all of it: a topic that only boosts critical has no cutoff on it.
	f.TopicRules["technology"] = TopicRules{Signals: Rules{Weights: map[string]float64{"critical": 2}}}
	if r := f.RulesFor("technology/ai"); r.Signals.Max["critical"] != 0 || len(r.Signals.Max) != 0 || r.Signals.Weights["critical"] != 2 {
		t.Errorf("a boost on its own replaces the cutoff too: %+v", r.Signals)
	}
	// The feed's own rules are never changed by working out a topic's.
	if f.Signals.Max["critical"] != 0.5 || len(f.Signals.Weights) != 0 {
		t.Errorf("the feed's rules changed: %+v", f.Signals)
	}
	if src := ruleSource(f.TopicRules, "us_politics/elections", "signal", "critical"); src != "us_politics/elections" {
		t.Errorf("source %q", src)
	}
	if src := ruleSource(f.TopicRules, "us_politics/congress", "signal", "critical"); src != "us_politics" {
		t.Errorf("source %q", src)
	}
	if src := ruleSource(f.TopicRules, "us_politics/congress", "tone", "outraged"); src != "" {
		t.Errorf("source %q", src)
	}
}

func TestTopicRulesBoostPostsOfTheirTopic(t *testing.T) {
	f := topicRulesFeed()
	funny := map[string]float32{"humorous": 1}
	meaty := map[string]float32{"substance": 1} // keeps both priors above their floor
	tech := Post{URI: "tech", TopPath: "technology/ai", IndexedAt: now, Tone: funny, Signals: meaty}
	election := Post{URI: "election", TopPath: "us_politics/elections", IndexedAt: now, Tone: funny, Signals: meaty}
	a, b := ScoreBreakdown(tech, f, now), ScoreBreakdown(election, f, now)
	if a.Prior-b.Prior != 2 {
		t.Errorf("humor lifts by 1 for tech and sinks by 1 for elections: %v vs %v", a.Prior, b.Prior)
	}
}

func TestTopicRulesValidation(t *testing.T) {
	paths := map[string]bool{"us_politics": true, "us_politics/elections": true, "technology": true}
	check := func(tr map[string]TopicRules) error {
		f := Feed{Rkey: "f", DisplayName: "F", Paths: []string{"technology"}, MinProb: 0.5, Ranking: DefaultRanking, TopicRules: tr}
		return (&Config{Feeds: []Feed{f}}).Validate(paths)
	}
	if err := check(topicRulesFeed().TopicRules); err != nil {
		t.Errorf("good rules: %v", err)
	}
	for name, tr := range map[string]map[string]TopicRules{
		"an unknown topic":   {"cooking": {Tone: Rules{Max: map[string]float32{"outraged": 0.5}}}},
		"an unknown score":   {"technology": {Tone: Rules{Max: map[string]float32{"sad": 0.5}}}},
		"a cutoff above 1":   {"technology": {Signals: Rules{Max: map[string]float32{"critical": 1.5}}}},
		"a boost past 10":    {"technology": {Signals: Rules{Weights: map[string]float64{"news": 11}}}},
		"a tone as a signal": {"technology": {Signals: Rules{Max: map[string]float32{"outraged": 0.5}}}},
	} {
		if err := check(tr); err == nil {
			t.Errorf("%s: accepted", name)
		}
	}
	many := map[string]TopicRules{}
	for i := 0; i <= maxTopicRules; i++ {
		many[strings.Repeat("x", i+1)] = TopicRules{}
	}
	if err := validateTopicRules(many, nil, checkRules); err == nil {
		t.Error("too many topics accepted")
	}
}

func TestTopicRulesSurviveBeingStored(t *testing.T) {
	f := topicRulesFeed()
	spec, err := SpecOf(f)
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(spec)
	if !strings.Contains(string(b), `"topic_rules":{"us_politics":{"signals":{"max":{"critical":1}}}`) {
		t.Errorf("stored as %s", b)
	}
	var back FeedSpec
	if err := json.Unmarshal(b, &back); err != nil {
		t.Fatal(err)
	}
	if got := back.Feed("", "f").RulesFor("us_politics/elections"); got.Signals.Max["critical"] != 0.2 {
		t.Errorf("after being stored: %+v", got)
	}
}

func TestCutoffConditionGroupsPostsByTopic(t *testing.T) {
	f := topicRulesFeed()
	sql, args := cutoffCondition(f)
	if strings.Count(sql, "?") != len(args) {
		t.Fatalf("%d placeholders, %d arguments: %s", strings.Count(sql, "?"), len(args), sql)
	}
	// One group for the subtopic, one for the rest of its broad topic, one for everything else.
	if !strings.HasPrefix(sql, " AND ((top_path = ?") || strings.Count(sql, ") OR (") != 2 {
		t.Errorf("groups: %s", sql)
	}
	// Without rules for topics, the conditions are as they always were.
	f.TopicRules = nil
	sql, args = cutoffCondition(f)
	if sql != " AND tone[?] <= ? AND signals[?] <= ? AND signals[?] >= ?" || len(args) != 6 {
		t.Errorf("plain: %s %v", sql, args)
	}
}

func TestTopicMinProb(t *testing.T) {
	f := Feed{Paths: []string{"sports", "technology/ai", "technology/software_dev", "us_politics/elections"}, MinProb: 0.5,
		TopicRules: map[string]TopicRules{
			"technology":            {MinProb: 0.3},
			"technology/ai":         {MinProb: 0.8},
			"us_politics/elections": {Tone: Rules{Max: map[string]float32{"outraged": 0.5}}}, // no min_prob of its own
		}}
	for path, want := range map[string]float32{"sports": 0.5, "technology/ai": 0.8, "technology/software_dev": 0.3, "us_politics/elections": 0.5} {
		if got := f.pathMinProb(path); got != want {
			t.Errorf("%s: %v, want %v", path, got, want)
		}
	}
	sql, args := matchCondition(f)
	if strings.Count(sql, " OR ") != 2 || strings.Count(sql, "?") != len(args) {
		t.Errorf("three thresholds, three groups: %s %v", sql, args)
	}
	// Without thresholds of their own, the one the feed has always had.
	f.TopicRules = nil
	if sql, args := matchCondition(f); sql != "score >= ?" || len(args) != 1 || args[0] != float32(0.5) {
		t.Errorf("plain: %s %v", sql, args)
	}
	bad := Feed{Rkey: "f", DisplayName: "F", Paths: []string{"technology"}, MinProb: 0.5, Ranking: DefaultRanking,
		TopicRules: map[string]TopicRules{"technology": {MinProb: 1.5}}}
	if err := (&Config{Feeds: []Feed{bad}}).Validate(map[string]bool{"technology": true}); err == nil {
		t.Error("a min_prob above 1 accepted")
	}

	// A personal feed: a topic's own min_prob, never under what the pool holds.
	cfg := PersonalConfigDefaults()
	tun := Tuning{TopicRules: map[string]TopicRules{"sports": {MinProb: 0.35}, "sports/baseball": {MinProb: 0.9}, "food": {MinProb: 0.1}}}
	c := tun.Config(cfg)
	for path, want := range map[string]float32{"sports/soccer": 0.35, "sports/baseball": 0.9, "food/baking": cfg.PoolMinTopicProb, "technology/ai": cfg.MinTopicProb} {
		if got := tun.MinTopicProbFor(c, path); got != want {
			t.Errorf("personal %s: %v, want %v", path, got, want)
		}
	}
	post := func(path string, p float32) Post { return Post{TopPath: path, TopPathP: p} }
	if tun.Excludes(post("sports/soccer", 0.4), c, now) || !tun.Excludes(post("technology/ai", 0.4), c, now) {
		t.Error("a soccer post at 0.4 is shown, an AI post at 0.4 isn't")
	}
}

func TestTuningTopicRules(t *testing.T) {
	cfg := PersonalConfigDefaults()
	tun := Tuning{
		Signals: Rules{Max: map[string]float32{"critical": 0.3}},
		TopicRules: map[string]TopicRules{
			"us_politics":         {Signals: Rules{Max: map[string]float32{"critical": 1}}},
			"gaming/video_games":  {Tone: Rules{Min: map[string]float32{"humorous": 0.5}}},
			"us_politics/foreign": {Signals: Rules{Weights: map[string]float64{"news": 2}}},
		},
	}
	if err := tun.check(); err != nil {
		t.Fatal(err)
	}
	critical := map[string]float32{"critical": 0.9}
	post := func(path string, tone map[string]float32) Post {
		return Post{TopPath: path, TopPathP: 0.9, Signals: critical, Tone: tone}
	}
	c := tun.Config(cfg)
	if !tun.Excludes(post("gaming/video_games", map[string]float32{"humorous": 0.6}), c, now) {
		t.Error("a funny but critical game post is cut: games have rules of their own only for humor")
	}
	if tun.Excludes(post("gaming/video_games", map[string]float32{"humorous": 0.6}), c, now) != !tun.Signals.Allows(critical) {
		t.Error("games follow the viewer's own rule on critical")
	}
	if tun.Excludes(post("us_politics/congress", map[string]float32{}), c, now) {
		t.Error("a critical politics post is let through")
	}
	// The subtopic's rules name only news, so critical is still its broad topic's: let through.
	if tun.Excludes(post("us_politics/foreign", map[string]float32{}), c, now) {
		t.Error("a subtopic keeps its broad topic's rules for scores its own don't name")
	}
	if !tun.Excludes(post("gaming/video_games", map[string]float32{"humorous": 0.2}), c, now) {
		t.Error("an unfunny game post is cut by the games' own minimum")
	}
	if !tun.customRanking() {
		t.Error("a topic's boost ranks the viewer's own way")
	}
	if tun.IsZero() {
		t.Error("topic rules are a change")
	}
	// Only cutoffs, and no boost of the viewer's own to take away: the shared pools still serve.
	cutsOnly := Tuning{TopicRules: map[string]TopicRules{"us_politics": {Signals: Rules{Max: map[string]float32{"critical": 0.5}}}}}
	if cutsOnly.customRanking() {
		t.Error("cutoffs alone don't change the ranking")
	}
	unboost := Tuning{Signals: Rules{Weights: map[string]float64{"news": 1}},
		TopicRules: map[string]TopicRules{"us_politics": {Signals: Rules{Max: map[string]float32{"news": 1}}}}}
	if !unboost.customRanking() {
		t.Error("a topic that takes away the viewer's boost ranks differently")
	}
	// A broad topic of an offered subtopic can have rules; a topic that isn't offered can't.
	offered := map[string]bool{"us_politics/congress": true, "us_politics/foreign": true, "gaming/video_games": true}
	if err := tun.Validate(offered); err != nil {
		t.Errorf("rules for a broad topic and subtopics: %v", err)
	}
	bad := Tuning{TopicRules: map[string]TopicRules{"adult_content": {Tone: Rules{Max: map[string]float32{"outraged": 0.5}}}}}
	if err := bad.Validate(offered); err == nil {
		t.Error("rules for a topic that isn't offered")
	}
	// Saved and read back as the page sends them.
	var saved Tuning
	if err := json.Unmarshal([]byte(`{"topicRules":{"us_politics":{"signals":{"max":{"critical":1}}}}}`), &saved); err != nil ||
		saved.rulesFor("us_politics/x").Signals.Max["critical"] != 1 {
		t.Errorf("%+v %v", saved, err)
	}
}
