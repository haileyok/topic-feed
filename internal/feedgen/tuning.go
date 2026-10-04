package feedgen

import (
	"fmt"
	"math"
	"slices"
	"strings"
	"time"
)

const (
	// MaxTopicWeight is the most a viewer can turn an interest up.
	MaxTopicWeight = 5.0
	// maxTuningTopics bounds how many topics a viewer can adjust or add.
	maxTuningTopics = 60

	// What "hide promotional posts" leaves out: posts the model scores above these.
	promoAdMax        = 0.3
	promoBaitMax      = 0.4
	promoSpamMax      = 0.1
	promoSelfPromoMax = 0.5
)

// The limits of what a viewer can set. Settings about the feed itself can only be made stricter
// than the feed's own where more would need posts the feed doesn't hold: a window of hours
// longer than the feed's, a list longer than its list size.
const (
	MaxGravity       = 10.0
	MaxFreshEvery    = 50
	MaxPromoPenalty  = 10.0
	MaxEngagement    = 20.0 // the weight of a like, repost, reply or quote
	MaxAuthorGap     = 50
	MaxLookbackDays  = 30
	MaxMinLikes      = 500
	MaxInterests     = 100
	MaxWindowHours   = 24 * 30
	MinWindowHours   = 0.5
	MaxServesSetting = 20
	MaxListSetting   = 1000
	// MaxBoost is the most a tone or signal can lift or sink a post's score.
	MaxBoost = 10.0
)

// Freshness settings: how much recent posts are favoured over popular ones.
const (
	FreshnessPopular  = "popular"
	FreshnessBalanced = "balanced"
	FreshnessFresh    = "fresh"
)

// popularGravity is the gravity of the "popular" freshness setting, the one that lets age count
// least. The pool's engaged posts are chosen with it (see PoolQuery), so that every setting finds
// what it wants among them.
const popularGravity = 1.2

// Tuning is how a viewer has adjusted their personal feed. The zero value changes nothing, and
// so does any setting left at zero or out: it is the feed's own.
type Tuning struct {
	// Topics adjusts interests by subtopic path. A weight multiplies the topic's share of the
	// feed: 0 mutes it, 1 leaves it as it is, up to MaxTopicWeight. A topic the viewer hasn't
	// liked anything in is added, starting as strong as their typical interest.
	Topics map[string]float64 `json:"topics,omitempty"`
	// Freshness is "popular", "balanced" (the feed's own ranking), or "fresh"; "" is balanced.
	// It sets the ranking's gravity and fresh slots; Ranking can then change those further.
	Freshness string `json:"freshness,omitempty"`
	// AuthorGap is how many slots apart one author's posts are kept; nil is the feed's setting.
	AuthorGap *int `json:"authorGap,omitempty"`
	// MinEngagement is how much engagement (likes, reposts, replies, and quotes at the feed's
	// ranking weights) a post needs before the feed shows it; nil is the feed's setting, and 0 is
	// a value: no minimum, so the newest posts fill whatever the others can't.
	MinEngagement *float64 `json:"minEngagement,omitempty"`

	// HalfLifeDays is how fast old likes fade: a like counts half as much every this many days.
	HalfLifeDays float64 `json:"halfLifeDays,omitempty"`
	// LookbackDays is how far back likes count.
	LookbackDays int `json:"lookbackDays,omitempty"`
	// MinLikes is how many liked, classified posts it takes for their likes to decide their
	// interests; with fewer they get a mix of every topic.
	MinLikes int `json:"minLikes,omitempty"`
	// Interests is how many interests the feed is built from, strongest first.
	Interests int `json:"interests,omitempty"`
	// WindowHours leaves out posts older than this, down to MinWindowHours; it can't reach further
	// back than the feed's.
	WindowHours float64 `json:"windowHours,omitempty"`
	// MinTopicProb is how sure the model has to be of a post's topic; it can't go below the feed's.
	MinTopicProb float64 `json:"minTopicProb,omitempty"`
	// MaxServes is how many times a post may be sent before it counts as seen.
	MaxServes int `json:"maxServes,omitempty"`
	// ListSize is how many posts the feed holds at a time; it can't be longer than the feed's.
	ListSize int `json:"listSize,omitempty"`

	// Ranking changes the ranking's numbers (see Ranking).
	Ranking *RankingTuning `json:"ranking,omitempty"`
	// HidePromo leaves out posts the model scores as ads, engagement bait, spam, or self-promotion.
	HidePromo bool `json:"hidePromo,omitempty"`
	// ShowSeen keeps posts the viewer has already seen in the feed: ones Bluesky reported as seen,
	// and ones the feed has sent MaxServes times. Their own posts and the ones they liked or
	// reposted are still left out. Off (the zero value) is the feed's own rule, which leaves seen
	// posts out.
	ShowSeen bool `json:"showSeen,omitempty"`
	// Tone and Signals are cutoffs and boosts on the model's scores, as in a feed's own
	// configuration (see Rules): Max and Min leave posts out, Weights lift or sink them.
	Tone    Rules `json:"tone,omitzero"`
	Signals Rules `json:"signals,omitzero"`
}

// RankingTuning changes the numbers a feed ranks with (see Ranking). A nil field is the feed's.
type RankingTuning struct {
	Gravity      *float64 `json:"gravity,omitempty"`
	FreshEvery   *int     `json:"freshEvery,omitempty"`
	PromoPenalty *float64 `json:"promoPenalty,omitempty"`
	Like         *float64 `json:"like,omitempty"`
	Repost       *float64 `json:"repost,omitempty"`
	Reply        *float64 `json:"reply,omitempty"`
	Quote        *float64 `json:"quote,omitempty"`
	// EngagementPower is how much popularity counts (see Ranking.EngagementPower).
	EngagementPower *float64 `json:"engagementPower,omitempty"`
}

func (r *RankingTuning) isZero() bool {
	return r == nil || *r == RankingTuning{}
}

// apply is base with the changes made.
func (r *RankingTuning) apply(base Ranking) Ranking {
	if r == nil {
		return base
	}
	set := func(dst *float64, v *float64) {
		if v != nil {
			*dst = *v
		}
	}
	set(&base.Gravity, r.Gravity)
	set(&base.PromoPenalty, r.PromoPenalty)
	set(&base.Weights.Like, r.Like)
	set(&base.Weights.Repost, r.Repost)
	set(&base.Weights.Reply, r.Reply)
	set(&base.Weights.Quote, r.Quote)
	set(&base.EngagementPower, r.EngagementPower)
	if r.FreshEvery != nil {
		base.FreshEvery = *r.FreshEvery
	}
	return base
}

func (r *RankingTuning) check() error {
	if r == nil {
		return nil
	}
	for name, v := range map[string]*float64{"gravity": r.Gravity, "promoPenalty": r.PromoPenalty} {
		limit := MaxGravity
		if name == "promoPenalty" {
			limit = MaxPromoPenalty
		}
		if v != nil && !(*v >= 0 && *v <= limit) { // also rejects NaN
			return fmt.Errorf("ranking %s must be between 0 and %g", name, limit)
		}
	}
	for name, v := range map[string]*float64{"like": r.Like, "repost": r.Repost, "reply": r.Reply, "quote": r.Quote} {
		if v != nil && !(*v >= 0 && *v <= MaxEngagement) {
			return fmt.Errorf("ranking %s must be between 0 and %g", name, MaxEngagement)
		}
	}
	if r.FreshEvery != nil && (*r.FreshEvery < 0 || *r.FreshEvery > MaxFreshEvery) {
		return fmt.Errorf("ranking freshEvery must be between 0 and %d", MaxFreshEvery)
	}
	if v := r.EngagementPower; v != nil && !(*v >= MinEngagementPower && *v <= 1) {
		return fmt.Errorf("ranking engagementPower must be between %g and 1", MinEngagementPower)
	}
	return nil
}

// Validate checks every setting. topics is the set of paths in the taxonomy; only subtopics
// (broad/sub) can be adjusted.
func (t Tuning) Validate(topics map[string]bool) error {
	if len(t.Topics) > maxTuningTopics {
		return fmt.Errorf("at most %d topics can be adjusted", maxTuningTopics)
	}
	for path := range t.Topics {
		if !strings.Contains(path, "/") || !topics[path] {
			return fmt.Errorf("%q is not a subtopic", path)
		}
	}
	return t.check()
}

// checkRules validates a feed's cutoffs and boosts the way a tuning needs them: every name is one
// of the model's scores, cutoffs are between 0 and 1 and don't leave nothing (a minimum above the
// maximum), and a boost is within MaxBoost either way. NaN is refused everywhere.
func checkRules(kind string, r Rules, names []string) error {
	known := func(name string) error {
		if !slices.Contains(names, name) {
			return fmt.Errorf("unknown %s %q (%ss: %s)", kind, name, kind, strings.Join(names, ", "))
		}
		return nil
	}
	for _, m := range []map[string]float32{r.Max, r.Min} {
		for name, v := range m {
			if err := known(name); err != nil {
				return err
			}
			if !(v >= 0 && v <= 1) {
				return fmt.Errorf("%s %s: cutoffs are between 0 and 1", kind, name)
			}
		}
	}
	for name, lo := range r.Min {
		if hi, ok := r.Max[name]; ok && lo > hi {
			return fmt.Errorf("%s %s: the minimum is above the maximum", kind, name)
		}
	}
	for name, w := range r.Weights {
		if err := known(name); err != nil {
			return err
		}
		if !(w >= -MaxBoost && w <= MaxBoost) {
			return fmt.Errorf("%s %s: boosts are between -%g and %g", kind, name, MaxBoost, MaxBoost)
		}
	}
	return nil
}

// check validates everything but which topics exist: the numbers must be in range, so a
// tuning that passes can't make the feed misbehave. A path that isn't in the taxonomy is
// harmless, it just matches no posts.
func (t Tuning) check() error {
	if len(t.Topics) > maxTuningTopics {
		return fmt.Errorf("at most %d topics can be adjusted", maxTuningTopics)
	}
	for path, w := range t.Topics {
		if !(w >= 0 && w <= MaxTopicWeight) { // also rejects NaN
			return fmt.Errorf("the weight for %s must be between 0 and %g", path, MaxTopicWeight)
		}
	}
	switch t.Freshness {
	case "", FreshnessPopular, FreshnessBalanced, FreshnessFresh:
	default:
		return fmt.Errorf("freshness must be %s, %s, or %s", FreshnessPopular, FreshnessBalanced, FreshnessFresh)
	}
	if t.AuthorGap != nil && (*t.AuthorGap < 0 || *t.AuthorGap > MaxAuthorGap) {
		return fmt.Errorf("authorGap must be between 0 and %d", MaxAuthorGap)
	}
	if t.MinEngagement != nil && !(*t.MinEngagement >= 0 && *t.MinEngagement <= MaxMinEngagement) {
		return fmt.Errorf("minEngagement must be between 0 and %g", MaxMinEngagement)
	}
	if t.HalfLifeDays != 0 && !(t.HalfLifeDays >= 1 && t.HalfLifeDays <= 30) {
		return fmt.Errorf("halfLifeDays must be between 1 and 30 (0 for the default)")
	}
	// Whole numbers where a feed's own setting is one: zero is "the feed's".
	for _, c := range []struct {
		name     string
		v, limit int
	}{
		{"lookbackDays", t.LookbackDays, MaxLookbackDays}, {"minLikes", t.MinLikes, MaxMinLikes}, {"interests", t.Interests, MaxInterests},
		{"maxServes", t.MaxServes, MaxServesSetting}, {"listSize", t.ListSize, MaxListSetting},
	} {
		if c.v != 0 && (c.v < 1 || c.v > c.limit) {
			return fmt.Errorf("%s must be between 1 and %d (0 for the feed's own)", c.name, c.limit)
		}
	}
	if t.WindowHours != 0 && !(t.WindowHours >= MinWindowHours && t.WindowHours <= MaxWindowHours) { // also rejects NaN
		return fmt.Errorf("windowHours must be between %g and %d (0 for the feed's own)", MinWindowHours, MaxWindowHours)
	}
	if t.MinTopicProb != 0 && !(t.MinTopicProb >= 0.05 && t.MinTopicProb <= 0.99) {
		return fmt.Errorf("minTopicProb must be between 0.05 and 0.99 (0 for the feed's own)")
	}
	if err := t.Ranking.check(); err != nil {
		return err
	}
	if err := checkRules("tone", t.Tone, Tones); err != nil {
		return err
	}
	return checkRules("signal", t.Signals, Signals)
}

// IsZero reports whether the tuning changes nothing.
func (t Tuning) IsZero() bool {
	return len(t.Topics) == 0 && (t.Freshness == "" || t.Freshness == FreshnessBalanced) && t.AuthorGap == nil && t.MinEngagement == nil &&
		t.HalfLifeDays == 0 && t.LookbackDays == 0 && t.MinLikes == 0 && t.Interests == 0 && t.WindowHours == 0 &&
		t.MinTopicProb == 0 && t.MaxServes == 0 && t.ListSize == 0 && t.Ranking.isZero() && !t.HidePromo && !t.ShowSeen &&
		t.Tone.IsZero() && t.Signals.IsZero()
}

// IsZero reports whether the rules say nothing.
func (r Rules) IsZero() bool { return len(r.Max) == 0 && len(r.Min) == 0 && len(r.Weights) == 0 }

// hasTopics reports whether any topic is turned on, which gives a viewer who hasn't liked enough
// yet interests of their own.
func (t Tuning) hasTopics() bool {
	for _, w := range t.Topics {
		if w > 0 {
			return true
		}
	}
	return false
}

// ApplyToMass adjusts how much of each subtopic a viewer liked by the tuning's topic weights.
// topN is how many interests the feed uses: an added topic starts as strong as the median of
// the viewer's top topN (or 1 when they have none), then its weight applies.
func (t Tuning) ApplyToMass(mass map[string]float64, topN int) map[string]float64 {
	if len(t.Topics) == 0 {
		return mass
	}
	out := make(map[string]float64, len(mass)+len(t.Topics))
	for path, m := range mass {
		if w, ok := t.Topics[path]; ok {
			m *= w
		}
		if m > 0 {
			out[path] = m
		}
	}
	base := medianMass(mass, topN)
	for path, w := range t.Topics {
		if _, liked := mass[path]; !liked && w > 0 {
			out[path] = base * w
		}
	}
	return out
}

// medianMass is the median of the topN largest masses, leaving out "unclear"; 1 if there are none.
func medianMass(mass map[string]float64, topN int) float64 {
	var ms []float64
	for path, m := range mass {
		if path != unclearTopic && m > 0 {
			ms = append(ms, m)
		}
	}
	if len(ms) == 0 {
		return 1
	}
	slices.SortFunc(ms, func(a, b float64) int {
		switch {
		case a > b:
			return -1
		case a < b:
			return 1
		}
		return 0
	})
	if len(ms) > topN {
		ms = ms[:topN]
	}
	if n := len(ms); n%2 == 1 {
		return ms[n/2]
	} else {
		return (ms[n/2-1] + ms[n/2]) / 2
	}
}

// RankingFor is the feed's ranking with the tuning's freshness, and then its own changes, applied.
func (t Tuning) RankingFor(base Ranking) Ranking {
	switch t.Freshness {
	case FreshnessPopular:
		base.Gravity, base.FreshEvery = popularGravity, 0
	case FreshnessFresh:
		base.Gravity, base.FreshEvery = 3, 3
	}
	return t.Ranking.apply(base)
}

// customRanking reports whether the tuning ranks posts in a way the feed doesn't already hold a
// ranked pool for: its own numbers, or boosts on tone and signals. (Freshness alone picks one of
// the pools the feed keeps.)
func (t Tuning) customRanking() bool {
	nonZero := func(m map[string]float64) bool {
		for _, w := range m {
			if w != 0 {
				return true
			}
		}
		return false
	}
	return !t.Ranking.isZero() || nonZero(t.Tone.Weights) || nonZero(t.Signals.Weights)
}

// Config is the personal feed's configuration with the viewer's settings applied.
func (t Tuning) Config(base PersonalConfig) PersonalConfig {
	c := base
	if base.AuthorGap != nil { // a copy of its own: nothing done to the result reaches the feed's configuration
		gap := *base.AuthorGap
		c.AuthorGap = &gap
	}
	if base.MinEngagement != nil {
		minEngagement := *base.MinEngagement
		c.MinEngagement = &minEngagement
	}
	if t.MinEngagement != nil {
		minEngagement := *t.MinEngagement
		c.MinEngagement = &minEngagement
	}
	if t.HalfLifeDays != 0 {
		c.HalfLifeDays = t.HalfLifeDays
	}
	if t.LookbackDays != 0 {
		c.LookbackDays = t.LookbackDays
	}
	if t.MinLikes != 0 {
		c.MinLikes = t.MinLikes
	}
	if t.Interests != 0 {
		c.Topics = t.Interests
	}
	if t.WindowHours != 0 {
		// Whole hours, rounded up: Excludes applies the exact window.
		c.WindowHours = min(c.WindowHours, int(math.Ceil(t.WindowHours)))
	}
	if t.MinTopicProb != 0 {
		c.MinTopicProb = max(c.MinTopicProb, float32(t.MinTopicProb))
	}
	if t.MaxServes != 0 {
		c.MaxServes = t.MaxServes
	}
	if t.ListSize != 0 {
		c.ListSize = min(c.ListSize, t.ListSize)
	}
	if t.AuthorGap != nil {
		gap := *t.AuthorGap
		c.AuthorGap = &gap
	}
	return c
}

// likeKey is what a reading of someone's likes depends on: when the tuning changes it, the likes
// have to be read again.
type likeKey struct {
	halfLifeDays float64
	lookbackDays int
}

func (t Tuning) likeKey(base PersonalConfig) likeKey {
	c := t.Config(base)
	return likeKey{c.HalfLifeDays, c.LookbackDays}
}

// Hides reports whether the tuning leaves a post out of the viewer's feed for what the model
// scored it.
func (t Tuning) Hides(p Post) bool {
	if t.HidePromo {
		s := p.Signals
		if s["ad"] > promoAdMax || s["engagement_bait"] > promoBaitMax || s["spam"] > promoSpamMax || s["self_promo"] > promoSelfPromoMax {
			return true
		}
	}
	return !t.Tone.Allows(p.Tone) || !t.Signals.Allows(p.Signals)
}

// belowMinEngagement reports whether a post has been reacted to less than cfg asks (its likes,
// reposts, replies, and quotes at weights w). A feed that has run out of posts people have
// reacted to is shorter, not padded with ones they haven't.
//
// w is the feed's own weights, never a viewer's: whether anyone has reacted to a post doesn't
// depend on how a viewer ranks, and a viewer who made every kind count for nothing in their
// ranking would otherwise have no post able to meet any minimum.
func belowMinEngagement(p Post, cfg PersonalConfig, w Weights) bool {
	return cfg.MinEngagement != nil && p.Engagement(w) < *cfg.MinEngagement
}

// Excludes reports whether the tuning leaves a post out of the viewer's feed: for what the model
// scored it (Hides), or because the viewer asked for newer posts or surer topics than the feed's
// own pool of posts holds. cfg is the configuration with the tuning applied (Config).
func (t Tuning) Excludes(p Post, cfg PersonalConfig, now time.Time) bool {
	if t.Hides(p) {
		return true
	}
	if t.WindowHours != 0 {
		window := min(float64(cfg.WindowHours), t.WindowHours)
		if p.IndexedAt.Before(now.Add(-time.Duration(window * float64(time.Hour)))) {
			return true
		}
	}
	return t.MinTopicProb != 0 && p.TopPathP < cfg.MinTopicProb
}
