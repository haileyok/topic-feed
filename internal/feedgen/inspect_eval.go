package feedgen

import (
	"fmt"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/haileyok/topic-feed/internal/labelpolicy"
)

// Check is one rule of a feed, and whether a post passes it.
type Check struct {
	Name   string `json:"name"`
	Pass   bool   `json:"pass"`
	Detail string `json:"detail"`
	// Soft marks a check about when the post is, not what it is: a post that fails it still
	// matched the feed while it was fresh enough.
	Soft bool `json:"soft,omitempty"`
}

// LiveStatus says whether a feed holds the post right now.
type LiveStatus struct {
	InBuild  bool      `json:"inBuild"`
	Position int       `json:"position,omitempty"` // 1-based, in the feed's order
	Total    int       `json:"total"`              // posts in the build
	BuiltAt  time.Time `json:"builtAt"`
	Why      string    `json:"why,omitempty"` // why a post that matches isn't in the build
}

// FeedVerdict is what one feed makes of a post.
type FeedVerdict struct {
	Rkey     string `json:"rkey"`
	Name     string `json:"name"`
	Personal bool   `json:"personal,omitempty"`
	// Matches is whether every rule of the feed lets the post in, age aside: whether the feed would
	// have taken it while it was fresh. Fresh is whether it is young enough for the feed's window now.
	Matches bool    `json:"matches"`
	Fresh   bool    `json:"fresh"`
	Reason  string  `json:"reason,omitempty"` // the first rule the post fails
	Checks  []Check `json:"checks"`
	// Match is how well the post fits the feed's topics (the highest probability among them).
	Match   float32     `json:"match"`
	Ranking *ScoreParts `json:"ranking,omitempty"` // its score in this feed's ranking, as of now
	Live    *LiveStatus `json:"live,omitempty"`
	Note    string      `json:"note,omitempty"`
}

// EvalInput is a post as the feeds judge it: what the pipeline stored, and what has happened to the
// post and its author since.
type EvalInput struct {
	Post           Post // its time, scores, labels as processed, and engagement
	Model          string
	FeedPolicy     string
	PathProbs      map[string]float32
	BroadProbs     map[string]float32
	Deleted        bool
	AuthorInactive bool
	// CurrentLabels are the labels the policy's labelers have on the post and its author now.
	CurrentLabels []string
	Policy        *labelpolicy.Policy
}

// num3 is a probability with as many decimals as it needs, up to three: a post a hair under a
// cutoff must not read as exactly on it.
func num3(v float32) string { return trimNum(strconv.FormatFloat(float64(v), 'f', 3, 32)) }

func trimNum(s string) string {
	if strings.Contains(s, ".") {
		s = strings.TrimRight(strings.TrimRight(s, "0"), ".")
	}
	if s == "" {
		return "0"
	}
	return s
}

// numVs is a value written so that it can't read the same as the limit it is compared with unless
// it is equal to it: where three decimals would round it onto the limit, it shows more.
func numVs(v, limit float32) string {
	for d := 3; d <= 8; d++ {
		a := trimNum(strconv.FormatFloat(float64(v), 'f', d, 32))
		if v == limit || a != trimNum(strconv.FormatFloat(float64(limit), 'f', d, 32)) {
			return a
		}
	}
	return strconv.FormatFloat(float64(v), 'g', -1, 32)
}

func probOf(in EvalInput, path string) float32 {
	if isBroad(path) {
		return in.BroadProbs[path]
	}
	return in.PathProbs[path]
}

// topicMatch is the best probability the post has for any of the feed's paths, as Store.Build
// computes it (a subtopic's own probability, or a broad topic's), and which path gave it.
func topicMatch(f Feed, in EvalInput) (best float32, bestPath string) {
	for _, p := range f.Paths {
		if p == AnyTopic {
			continue
		}
		if v := probOf(in, p); v > best || bestPath == "" {
			best, bestPath = v, p
		}
	}
	return best, bestPath
}

// labelsCheck applies the label policy to the labels the post had when processed and the ones on it
// and its author now, as the feed does when it is built.
func labelsCheck(f Feed, in EvalInput) Check {
	all := slices.Concat(in.Post.Labels, in.CurrentLabels)
	decision := labelpolicy.OK
	if in.Policy != nil {
		decision = in.Policy.Decide(all)
	}
	c := Check{Name: "Labels on the post and its author"}
	labels := "no labels"
	if len(all) > 0 {
		labels = strings.Join(slices.Compact(slices.Sorted(slices.Values(all))), ", ")
	}
	switch decision {
	case labelpolicy.OK:
		c.Pass, c.Detail = true, labels+": allowed"
	case labelpolicy.AdultOnly:
		c.Pass = f.AllowAdult
		if c.Pass {
			c.Detail = labels + ": adult-only, and this feed allows adult posts"
		} else {
			c.Detail = labels + ": adult-only, and this feed doesn't allow adult posts"
		}
	default:
		c.Detail = labels + ": the label policy never shows these posts"
	}
	return c
}

// commonChecks are the checks every feed makes of a post: that the model scored it, that nobody has
// deleted it or lost their account, and the labels now on it.
func commonChecks(f Feed, in EvalInput) (classified, deleted, active Check) {
	classified = Check{Name: "Scored by the topic model", Pass: in.Model != ""}
	if in.Model != "" {
		classified.Detail = "scored by " + in.Model
	} else {
		classified.Detail = "never scored: the pipeline dropped it (see the label policy) or never processed it"
	}
	deleted = Check{Name: "Not deleted", Pass: !in.Deleted, Detail: "still there"}
	if in.Deleted {
		deleted.Detail = "the author deleted it"
	}
	active = Check{Name: "Author's account is active", Pass: !in.AuthorInactive, Detail: "active"}
	if in.AuthorInactive {
		active.Detail = "deactivated, suspended or taken down"
	}
	return classified, deleted, active
}

// EvaluateFeed runs the rules of a feed built from topic paths (Store.Build) against one post, one
// check at a time, so the page can say why a post is in or out. window is how far back the feed
// reaches. It does not look at ClickHouse: the checks are the SQL's conditions, restated.
func EvaluateFeed(f Feed, in EvalInput, now time.Time, window time.Duration) FeedVerdict {
	v := FeedVerdict{Rkey: f.Rkey, Name: f.DisplayName}
	classified, deleted, active := commonChecks(f, in)
	v.Checks = append(v.Checks, classified)

	// The policy as it was when the post was processed; labels now are checked below.
	policy := Check{Name: "Label policy when it was processed", Pass: in.FeedPolicy == labelpolicy.OK || (f.AllowAdult && in.FeedPolicy == labelpolicy.AdultOnly)}
	switch {
	case in.FeedPolicy == labelpolicy.OK:
		policy.Detail = "ok"
	case in.FeedPolicy == labelpolicy.AdultOnly && f.AllowAdult:
		policy.Detail = "adult-only, and this feed allows adult posts"
	case in.FeedPolicy == labelpolicy.AdultOnly:
		policy.Detail = "adult-only, and this feed doesn't allow adult posts"
	case in.FeedPolicy == "":
		policy.Detail = "not processed"
	default:
		policy.Detail = in.FeedPolicy + ": the label policy never shows these posts"
	}
	v.Checks = append(v.Checks, policy)

	// The topic.
	topic := Check{Name: "Topic"}
	if f.anyTopic() {
		v.Match = 1
		topic.Pass, topic.Detail = true, "this feed takes posts about any topic"
	} else {
		best, path := topicMatch(f, in)
		v.Match = best
		topic.Pass = best >= f.MinProb
		switch {
		case best == 0:
			topic.Detail = fmt.Sprintf("none of the feed's topics (%s) is among the topics the model scored for this post; it needs at least %s", strings.Join(f.Paths, ", "), num3(f.MinProb))
		case topic.Pass:
			topic.Detail = fmt.Sprintf("%s at %s, and the feed needs at least %s", path, numVs(best, f.MinProb), num3(f.MinProb))
		default:
			topic.Detail = fmt.Sprintf("best match is %s at %s, and the feed needs at least %s", path, numVs(best, f.MinProb), num3(f.MinProb))
		}
	}
	v.Checks = append(v.Checks, topic)

	// Topics the feed leaves out.
	for _, p := range slices.Sorted(mapKeys(f.Exclude)) {
		limit, got := f.Exclude[p], probOf(in, p)
		c := Check{Name: "Not about " + p, Pass: got <= limit}
		c.Detail = fmt.Sprintf("%s, and the feed allows at most %s", numVs(got, limit), num3(limit))
		v.Checks = append(v.Checks, c)
	}

	// Cutoffs on tone and signals: the feed's own, or its rules for the post's topic.
	rules := f.RulesFor(in.Post.TopPath)
	for _, r := range []struct {
		kind, key string
		rules     Rules
		scores    map[string]float32
	}{{"Tone", "tone", rules.Tone, in.Post.Tone}, {"Signal", "signal", rules.Signals, in.Post.Signals}} {
		// for says which topic's rules a cutoff is, when it isn't the feed's own.
		forTopic := func(name string) string {
			if src := ruleSource(f.TopicRules, in.Post.TopPath, r.key, name); src != "" {
				return fmt.Sprintf(" (the feed's rule for %s)", src)
			}
			return ""
		}
		for _, name := range slices.Sorted(mapKeys(r.rules.Max)) {
			limit, got := r.rules.Max[name], r.scores[name]
			if limit >= 1 {
				continue // lets every post through: a topic's rule lifting the feed's own cutoff
			}
			v.Checks = append(v.Checks, Check{Name: fmt.Sprintf("%s: %s at most %s%s", r.kind, name, num3(limit), forTopic(name)), Pass: got <= limit, Detail: numVs(got, limit)})
		}
		for _, name := range slices.Sorted(mapKeys(r.rules.Min)) {
			limit, got := r.rules.Min[name], r.scores[name]
			v.Checks = append(v.Checks, Check{Name: fmt.Sprintf("%s: %s at least %s%s", r.kind, name, num3(limit), forTopic(name)), Pass: got >= limit, Detail: numVs(got, limit)})
		}
	}

	v.Checks = append(v.Checks, deleted, active, labelsCheck(f, in))

	// When it is. This is the only check about time; the feed holds posts this young.
	v.Fresh = !in.Post.IndexedAt.Before(now.Add(-window))
	age := now.Sub(in.Post.IndexedAt).Round(time.Minute)
	fresh := Check{Name: "Within the feed's window", Pass: v.Fresh, Soft: true}
	if v.Fresh {
		fresh.Detail = fmt.Sprintf("posted %s ago, and the feed reaches back %s", age, window)
	} else {
		fresh.Detail = fmt.Sprintf("posted %s ago, but the feed only reaches back %s: it matched while it was fresh enough", age, window)
	}
	v.Checks = append(v.Checks, fresh)

	finishVerdict(&v)
	parts := ScoreBreakdown(in.Post, f, now)
	v.Ranking = &parts
	return v
}

// finishVerdict sets Matches and Reason from the checks.
func finishVerdict(v *FeedVerdict) {
	v.Matches = true
	for _, c := range v.Checks {
		if c.Soft || c.Pass {
			continue
		}
		if v.Matches {
			v.Reason = c.Name + ": " + c.Detail
		}
		v.Matches = false
	}
}

// EvaluatePersonal is what a personal feed (For you) makes of a post: whether it can be in the pool
// of posts that feed picks from for any viewer (store.go's poolFilter), and passes its minimum of
// reactions. Whether a viewer is shown it depends on their interests, what they have seen and the
// settings they tuned, which is not asked here.
func EvaluatePersonal(f Feed, in EvalInput, now time.Time) FeedVerdict {
	cfg := PersonalConfigDefaults()
	if f.Personal != nil {
		cfg = *f.Personal
	}
	v := FeedVerdict{Rkey: f.Rkey, Name: f.DisplayName, Personal: true}
	classified, deleted, active := commonChecks(f, in)
	v.Checks = append(v.Checks, classified)

	policy := Check{Name: "Label policy when it was processed", Pass: in.FeedPolicy == labelpolicy.OK}
	if policy.Pass {
		policy.Detail = "ok"
	} else if in.FeedPolicy == "" {
		policy.Detail = "not processed"
	} else {
		policy.Detail = in.FeedPolicy + ": a personal feed only picks from posts the policy marks ok"
	}
	v.Checks = append(v.Checks, policy)

	// A post counts for its most likely subtopic, when the model is sure enough of it.
	topPath, topP := in.Post.TopPath, in.Post.TopPathP
	topic := Check{Name: "Belongs to a subtopic", Pass: topPath != "" && topPath != unclearTopic && topP >= float32(cfg.MinTopicProb)}
	switch {
	case topPath == "":
		topic.Detail = "no topic"
	case topPath == unclearTopic:
		topic.Detail = fmt.Sprintf("the model's best guess is %q, which isn't a topic a viewer can have", unclearTopic)
	case topic.Pass:
		topic.Detail = fmt.Sprintf("%s at %s, and the feed needs at least %s", topPath, numVs(topP, float32(cfg.MinTopicProb)), num3(float32(cfg.MinTopicProb)))
	default:
		topic.Detail = fmt.Sprintf("most likely %s at %s, under the %s the feed needs", topPath, numVs(topP, float32(cfg.MinTopicProb)), num3(float32(cfg.MinTopicProb)))
	}
	v.Match = topP
	v.Checks = append(v.Checks, topic)

	v.Checks = append(v.Checks, deleted, active)
	// The pool takes only posts the policy marks ok, whatever labels they have now.
	lc := labelsCheck(Feed{}, in)
	v.Checks = append(v.Checks, lc)

	if cfg.MinEngagement != nil {
		eng := in.Post.Engagement(f.Ranking.Weights)
		min := *cfg.MinEngagement
		c := Check{Name: "Enough reactions", Pass: eng >= min}
		c.Detail = fmt.Sprintf("%s likes' worth (%d likes, %d reposts, %d replies, %d quotes at the feed's weights), and the feed needs at least %s",
			strconv.FormatFloat(eng, 'f', -1, 64), in.Post.Likes, in.Post.Reposts, in.Post.Replies, in.Post.Quotes, strconv.FormatFloat(min, 'f', -1, 64))
		v.Checks = append(v.Checks, c)
	}

	window := time.Duration(cfg.WindowHours) * time.Hour
	v.Fresh = !in.Post.IndexedAt.Before(now.Add(-window))
	age := now.Sub(in.Post.IndexedAt).Round(time.Minute)
	fresh := Check{Name: "Within the feed's window", Pass: v.Fresh, Soft: true}
	if v.Fresh {
		fresh.Detail = fmt.Sprintf("posted %s ago, and the feed reaches back %s", age, window)
	} else {
		fresh.Detail = fmt.Sprintf("posted %s ago, but the feed only reaches back %s: it was eligible while it was fresh enough", age, window)
	}
	v.Checks = append(v.Checks, fresh)

	finishVerdict(&v)
	v.Note = "Which viewers are shown it depends on their interests, what they have already seen and the settings they tuned."
	parts := ScoreBreakdown(in.Post, f, now)
	v.Ranking = &parts
	return v
}

// LiveStatusIn says whether a feed's current build holds the post, where, and if not why not.
// posts is the build in feed order, as Feeds.Posts returns it; limit is how many posts the feed keeps.
func LiveStatusIn(posts []Post, builtAt time.Time, uri string, v FeedVerdict, indexedAt time.Time, limit int, window time.Duration) *LiveStatus {
	ls := &LiveStatus{Total: len(posts), BuiltAt: builtAt}
	if builtAt.IsZero() {
		ls.Why = "the feed hasn't been built yet"
		return ls
	}
	for i, p := range posts {
		if p.URI == uri {
			ls.InBuild, ls.Position = true, i+1
			return ls
		}
	}
	switch {
	case !v.Matches:
		// Not in it, and not expected to be: the checks say why.
	case !v.Fresh:
		ls.Why = fmt.Sprintf("it is older than the %s the feed reaches back", window)
	default:
		if oldest, ok := oldestOf(posts); ok && indexedAt.Before(oldest) {
			ls.Why = fmt.Sprintf("the feed keeps only the %d newest posts that match, and this one is older than the oldest it holds", limit)
		} else {
			ls.Why = "it matches but isn't in the current build: it may have arrived after the build, or the build is still catching up"
		}
	}
	return ls
}

func oldestOf(posts []Post) (time.Time, bool) {
	if len(posts) == 0 {
		return time.Time{}, false
	}
	oldest := posts[0].IndexedAt
	for _, p := range posts[1:] {
		if p.IndexedAt.Before(oldest) {
			oldest = p.IndexedAt
		}
	}
	return oldest, true
}

func mapKeys[M ~map[K]V, K comparable, V any](m M) func(yield func(K) bool) {
	return func(yield func(K) bool) {
		for k := range m {
			if !yield(k) {
				return
			}
		}
	}
}
