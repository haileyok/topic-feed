package feedgen

import (
	"encoding/json"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/haileyok/topic-feed/internal/taxonomy"
)

func taxonomyPaths(t *testing.T) map[string]bool {
	t.Helper()
	tax, err := taxonomy.Load("../../taxonomy/v1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	return TaxonomyPaths(tax)
}

func ptr[T any](v T) *T { return &v }

func TestTuningValidate(t *testing.T) {
	paths := taxonomyPaths(t)
	good := []Tuning{
		{},
		{Topics: map[string]float64{"technology/ai": 0, "animals_nature/cats": 5, "sports/baseball": 1.5}},
		{Freshness: FreshnessFresh, AuthorGap: ptr(0), HalfLifeDays: 1, HidePromo: true,
			Tone: Rules{Max: map[string]float32{"outraged": 1}}, Signals: Rules{Min: map[string]float32{"substance": 0.9}}},
		{AuthorGap: ptr(50), HalfLifeDays: 30},
		// Every setting at its largest and smallest.
		{LookbackDays: 30, MinLikes: 500, Interests: 100, WindowHours: 720, MinTopicProb: 0.99, MaxServes: 20, ListSize: 1000,
			Ranking: &RankingTuning{Gravity: ptr(10.0), FreshEvery: ptr(50), PromoPenalty: ptr(10.0), Like: ptr(20.0), Repost: ptr(20.0), Reply: ptr(20.0), Quote: ptr(20.0)},
			Tone:    Rules{Weights: map[string]float64{"humorous": 10, "outraged": -10}}, Signals: Rules{Weights: map[string]float64{"substance": 10, "spam": -10}}},
		{LookbackDays: 1, MinLikes: 1, Interests: 1, WindowHours: 1, MinTopicProb: 0.05, MaxServes: 1, ListSize: 1,
			Ranking: &RankingTuning{Gravity: ptr(0.0), FreshEvery: ptr(0), PromoPenalty: ptr(0.0), Like: ptr(0.0), Repost: ptr(0.0), Reply: ptr(0.0), Quote: ptr(0.0)},
			Tone:    Rules{Min: map[string]float32{"informative": 0}, Max: map[string]float32{"informative": 0}}},
		{Tone: Rules{Min: map[string]float32{"humorous": 0.2}, Max: map[string]float32{"humorous": 0.2}}}, // the same: allowed
		{Signals: Rules{Max: map[string]float32{"ad": 0.3, "engagement_bait": 0.4, "spam": 0.1, "self_promo": 0.5, "critical": 1}}},
	}
	for i, g := range good {
		if err := g.Validate(paths); err != nil {
			t.Errorf("good %d: %v", i, err)
		}
	}
	many := map[string]float64{}
	for path := range paths {
		if strings.Contains(path, "/") {
			many[path] = 1
		}
	}
	bad := map[string]Tuning{
		"a topic that doesn't exist": {Topics: map[string]float64{"technology/nope": 1}},
		"a broad topic":              {Topics: map[string]float64{"technology": 1}},
		"unclear":                    {Topics: map[string]float64{"unclear": 1}},
		"a negative weight":          {Topics: map[string]float64{"technology/ai": -1}},
		"a weight above the maximum": {Topics: map[string]float64{"technology/ai": 5.01}},
		"a NaN weight":               {Topics: map[string]float64{"technology/ai": math.NaN()}},
		"too many topics":            {Topics: many},
		"unknown freshness":          {Freshness: "frozen"},
		"a negative author gap":      {AuthorGap: ptr(-1)},
		"an author gap above 50":     {AuthorGap: ptr(51)},
		"a half-life below a day":    {HalfLifeDays: 0.5},
		"a half-life above 30":       {HalfLifeDays: 31},
		"a NaN half-life":            {HalfLifeDays: math.NaN()},
		"an infinite half-life":      {HalfLifeDays: math.Inf(1)},

		"a negative tone maximum":              {Tone: Rules{Max: map[string]float32{"outraged": -0.1}}},
		"a tone maximum above 1":               {Tone: Rules{Max: map[string]float32{"outraged": 1.5}}},
		"a NaN tone maximum":                   {Tone: Rules{Max: map[string]float32{"outraged": float32(math.NaN())}}},
		"a negative signal minimum":            {Signals: Rules{Min: map[string]float32{"substance": -0.1}}},
		"a signal minimum above 1":             {Signals: Rules{Min: map[string]float32{"substance": 1.01}}},
		"a NaN signal minimum":                 {Signals: Rules{Min: map[string]float32{"substance": float32(math.NaN())}}},
		"a tone that doesn't exist":            {Tone: Rules{Max: map[string]float32{"sarcastic": 0.5}}},
		"a signal that doesn't exist":          {Signals: Rules{Min: map[string]float32{"vibes": 0.5}}},
		"a signal used as a tone":              {Tone: Rules{Max: map[string]float32{"substance": 0.5}}},
		"a tone used as a signal":              {Signals: Rules{Max: map[string]float32{"outraged": 0.5}}},
		"a boost on a tone that doesn't exist": {Tone: Rules{Weights: map[string]float64{"sarcastic": 1}}},
		"a minimum above the maximum":          {Tone: Rules{Min: map[string]float32{"outraged": 0.6}, Max: map[string]float32{"outraged": 0.5}}},
		"a boost above the limit":              {Signals: Rules{Weights: map[string]float64{"substance": 10.5}}},
		"a boost below the limit":              {Tone: Rules{Weights: map[string]float64{"humorous": -10.5}}},
		"a NaN boost":                          {Tone: Rules{Weights: map[string]float64{"humorous": math.NaN()}}},
		"an infinite boost":                    {Signals: Rules{Weights: map[string]float64{"spam": math.Inf(1)}}},

		"a negative gravity":          {Ranking: &RankingTuning{Gravity: ptr(-0.1)}},
		"gravity above the limit":     {Ranking: &RankingTuning{Gravity: ptr(10.1)}},
		"a NaN gravity":               {Ranking: &RankingTuning{Gravity: ptr(math.NaN())}},
		"negative fresh slots":        {Ranking: &RankingTuning{FreshEvery: ptr(-1)}},
		"fresh slots above the limit": {Ranking: &RankingTuning{FreshEvery: ptr(51)}},
		"a negative promo penalty":    {Ranking: &RankingTuning{PromoPenalty: ptr(-1.0)}},
		"a promo penalty too big":     {Ranking: &RankingTuning{PromoPenalty: ptr(10.5)}},
		"a negative like weight":      {Ranking: &RankingTuning{Like: ptr(-1.0)}},
		"a repost weight too big":     {Ranking: &RankingTuning{Repost: ptr(21.0)}},
		"a NaN reply weight":          {Ranking: &RankingTuning{Reply: ptr(math.NaN())}},
		"an infinite quote weight":    {Ranking: &RankingTuning{Quote: ptr(math.Inf(1))}},

		"a negative lookback":           {LookbackDays: -1},
		"a lookback above 30":           {LookbackDays: 31},
		"a negative minimum of likes":   {MinLikes: -1},
		"a minimum of likes too big":    {MinLikes: 501},
		"a negative interest count":     {Interests: -1},
		"too many interests":            {Interests: 101},
		"a negative window":             {WindowHours: -1},
		"a window longer than a month":  {WindowHours: 721},
		"a topic probability of 0.04":   {MinTopicProb: 0.04},
		"a topic probability of 1":      {MinTopicProb: 1},
		"a negative topic probability":  {MinTopicProb: -0.5},
		"a NaN topic probability":       {MinTopicProb: math.NaN()},
		"negative serves":               {MaxServes: -1},
		"too many serves":               {MaxServes: 21},
		"a negative list size":          {ListSize: -1},
		"a list size above 1000":        {ListSize: 1001},
		"a negative minimum engagement": {MinEngagement: ptr(-1.0)},
		"a minimum engagement too big":  {MinEngagement: ptr(MaxMinEngagement + 1)},
		"a NaN minimum engagement":      {MinEngagement: ptr(math.NaN())},
	}
	if len(many) <= maxTuningTopics {
		t.Fatalf("the taxonomy has only %d subtopics: the too-many case doesn't test anything", len(many))
	}
	for name, b := range bad {
		if err := b.Validate(paths); err == nil {
			t.Errorf("accepted %s", name)
		}
	}
}

func TestTuningIsZero(t *testing.T) {
	for _, z := range []Tuning{
		{}, {Freshness: FreshnessBalanced}, {Topics: map[string]float64{}}, {Ranking: &RankingTuning{}},
		{Tone: Rules{Max: map[string]float32{}}, Signals: Rules{Min: map[string]float32{}, Weights: map[string]float64{}}},
	} {
		if !z.IsZero() {
			t.Errorf("%+v should change nothing", z)
		}
	}
	for _, nz := range []Tuning{
		{Topics: map[string]float64{"technology/ai": 1}}, {Freshness: FreshnessFresh}, {AuthorGap: ptr(0)},
		{HalfLifeDays: 7}, {HidePromo: true}, {ShowSeen: true},
		{LookbackDays: 14}, {MinLikes: 3}, {Interests: 5}, {WindowHours: 12}, {MinTopicProb: 0.6}, {MaxServes: 1}, {ListSize: 50},
		{Ranking: &RankingTuning{Gravity: ptr(0.0)}}, {Ranking: &RankingTuning{FreshEvery: ptr(0)}}, {Ranking: &RankingTuning{Quote: ptr(0.0)}},
		{Tone: Rules{Max: map[string]float32{"outraged": 0.5}}}, {Tone: Rules{Min: map[string]float32{"informative": 0.1}}},
		{Tone: Rules{Weights: map[string]float64{"humorous": 1}}}, {Signals: Rules{Min: map[string]float32{"substance": 0.1}}},
		{Signals: Rules{Max: map[string]float32{"ad": 0.2}}}, {Signals: Rules{Weights: map[string]float64{"spam": -1}}},
	} {
		if nz.IsZero() {
			t.Errorf("%+v changes something", nz)
		}
	}
}

func TestApplyToMass(t *testing.T) {
	mass := map[string]float64{"a/x": 8, "b/x": 4, "c/x": 2, "d/x": 1, unclearTopic: 100}
	same := Tuning{}.ApplyToMass(mass, 20)
	if len(same) != len(mass) || same["a/x"] != 8 {
		t.Errorf("no tuning: %v", same)
	}

	got := Tuning{Topics: map[string]float64{
		"a/x": 0, // muted
		"b/x": 2, // turned up
		"c/x": 1, // unchanged
		"e/x": 2, // added
		"f/x": 0, // a muted topic that was never liked: nothing to add
	}}.ApplyToMass(mass, 20)
	if _, ok := got["a/x"]; ok {
		t.Error("a muted interest should be gone")
	}
	if got["b/x"] != 8 || got["c/x"] != 2 || got["d/x"] != 1 {
		t.Errorf("weights multiply the mass: %v", got)
	}
	// The median of 8, 4, 2, 1 is 3 ("unclear" doesn't count); added at weight 2 is 6.
	if got["e/x"] != 6 {
		t.Errorf("added topic mass %v, want the median interest (3) times 2", got["e/x"])
	}
	if _, ok := got["f/x"]; ok {
		t.Error("muting a topic that was never liked adds nothing")
	}
	if mass["a/x"] != 8 {
		t.Error("the input was changed")
	}

	// Only the strongest topN set the median.
	if m := medianMass(mass, 2); m != 6 {
		t.Errorf("median of the top 2 is %v, want 6", m)
	}
	// No likes at all: an added topic starts from 1.
	if got := (Tuning{Topics: map[string]float64{"a/x": 3}}).ApplyToMass(nil, 20); got["a/x"] != 3 {
		t.Errorf("added with no likes: %v", got)
	}
	if got := (Tuning{Topics: map[string]float64{"a/x": 3}}).ApplyToMass(map[string]float64{unclearTopic: 9}, 20); got["a/x"] != 3 {
		t.Errorf("only unclear likes: %v", got)
	}
}

func TestTuningRankingFor(t *testing.T) {
	base := DefaultRanking
	if (Tuning{}).RankingFor(base) != base || (Tuning{Freshness: FreshnessBalanced}).RankingFor(base) != base {
		t.Error("balanced is the feed's own ranking")
	}
	if r := (Tuning{Freshness: FreshnessPopular}).RankingFor(base); r.Gravity != 1.2 || r.FreshEvery != 0 || r.Weights != base.Weights || r.AuthorGap != base.AuthorGap {
		t.Errorf("popular: %+v", r)
	}
	if r := (Tuning{Freshness: FreshnessFresh}).RankingFor(base); r.Gravity != 3 || r.FreshEvery != 3 || r.PromoPenalty != base.PromoPenalty {
		t.Errorf("fresh: %+v", r)
	}
	// The viewer's own numbers come after the freshness setting, and only those they set change.
	own := &RankingTuning{Gravity: ptr(0.5), FreshEvery: ptr(0), PromoPenalty: ptr(4.0), Like: ptr(0.0), Quote: ptr(9.0)}
	r := (Tuning{Freshness: FreshnessFresh, Ranking: own}).RankingFor(base)
	want := base
	want.Gravity, want.FreshEvery, want.PromoPenalty, want.Weights.Like, want.Weights.Quote = 0.5, 0, 4, 0, 9
	if r != want {
		t.Errorf("own numbers over fresh: %+v, want %+v", r, want)
	}
	if r := (Tuning{Freshness: FreshnessPopular, Ranking: &RankingTuning{Repost: ptr(7.0)}}).RankingFor(base); r.Gravity != 1.2 || r.FreshEvery != 0 || r.Weights.Repost != 7 || r.Weights.Like != base.Weights.Like {
		t.Errorf("one number over popular: %+v", r)
	}
	if base != DefaultRanking {
		t.Error("the feed's ranking was changed")
	}
}

func TestTuningCustomRanking(t *testing.T) {
	for name, tune := range map[string]Tuning{
		"nothing":                     {},
		"freshness, which has pools":  {Freshness: FreshnessFresh},
		"filters":                     {HidePromo: true, Tone: Rules{Max: map[string]float32{"outraged": 0.5}}, Signals: Rules{Min: map[string]float32{"substance": 0.5}}},
		"topics and feed settings":    {Topics: map[string]float64{"a/x": 2}, AuthorGap: ptr(3), ListSize: 10, WindowHours: 6},
		"boosts of zero":              {Tone: Rules{Weights: map[string]float64{"humorous": 0}}},
		"an empty set of own numbers": {Ranking: &RankingTuning{}},
	} {
		if tune.customRanking() {
			t.Errorf("%s needs no ranked pool of its own", name)
		}
	}
	for name, tune := range map[string]Tuning{
		"a gravity":       {Ranking: &RankingTuning{Gravity: ptr(0.0)}},
		"fresh slots":     {Ranking: &RankingTuning{FreshEvery: ptr(0)}},
		"a promo penalty": {Ranking: &RankingTuning{PromoPenalty: ptr(0.0)}},
		"an engagement":   {Ranking: &RankingTuning{Reply: ptr(0.0)}},
		"a tone boost":    {Tone: Rules{Weights: map[string]float64{"humorous": 0.5}}},
		"a signal boost":  {Signals: Rules{Weights: map[string]float64{"spam": -1}}},
	} {
		if !tune.customRanking() {
			t.Errorf("%s changes how posts rank", name)
		}
	}
}

func TestTuningConfig(t *testing.T) {
	base := PersonalConfigDefaults()
	if got := (Tuning{}).Config(base); !reflect.DeepEqual(got, base) {
		t.Errorf("no tuning changes nothing: %+v", got)
	}
	got := Tuning{HalfLifeDays: 3, LookbackDays: 10, MinLikes: 9, Interests: 4, MaxServes: 1, AuthorGap: ptr(0)}.Config(base)
	if got.HalfLifeDays != 3 || got.LookbackDays != 10 || got.MinLikes != 9 || got.Topics != 4 || got.MaxServes != 1 || *got.AuthorGap != 0 {
		t.Errorf("%+v", got)
	}
	// Some settings can only be made stricter than the feed's own (the pool holds nothing past them).
	if got := (Tuning{WindowHours: 6}).Config(base); got.WindowHours != 6 {
		t.Errorf("a shorter window: %d", got.WindowHours)
	}
	if got := (Tuning{WindowHours: 700}).Config(base); got.WindowHours != base.WindowHours {
		t.Errorf("a window longer than the feed's: %d", got.WindowHours)
	}
	if got := (Tuning{ListSize: 40}).Config(base); got.ListSize != 40 {
		t.Errorf("a shorter list: %d", got.ListSize)
	}
	if got := (Tuning{ListSize: 900}).Config(base); got.ListSize != base.ListSize {
		t.Errorf("a longer list than the feed's: %d", got.ListSize)
	}
	if got := (Tuning{MinTopicProb: 0.8}).Config(base); got.MinTopicProb != 0.8 {
		t.Errorf("a surer topic: %v", got.MinTopicProb)
	}
	if got := (Tuning{MinTopicProb: 0.4}).Config(base); got.MinTopicProb != 0.4 {
		t.Errorf("a less sure topic than the feed's own, which the pool holds: %v", got.MinTopicProb)
	}
	if got := (Tuning{MinTopicProb: 0.2}).Config(base); got.MinTopicProb != base.PoolMinTopicProb {
		t.Errorf("a less sure topic than the pool holds: %v", got.MinTopicProb)
	}
	// The feed's own configuration is never changed through the copy.
	gap := *base.AuthorGap
	for name, tune := range map[string]Tuning{"a tuning with its own gap": {AuthorGap: ptr(1)}, "a tuning without": {}} {
		c := tune.Config(base)
		*c.AuthorGap = 99
		if *base.AuthorGap != gap {
			t.Fatalf("%s: the feed's author gap was changed", name)
		}
	}
}

func TestTuningLikeKey(t *testing.T) {
	base := PersonalConfigDefaults()
	same := Tuning{}.likeKey(base)
	if same != (likeKey{base.HalfLifeDays, base.LookbackDays}) {
		t.Errorf("%+v", same)
	}
	for name, tune := range map[string]Tuning{"a half-life": {HalfLifeDays: 2}, "a lookback": {LookbackDays: 3}} {
		if tune.likeKey(base) == same {
			t.Errorf("%s changes how likes are read", name)
		}
	}
	for name, tune := range map[string]Tuning{"topics": {Topics: map[string]float64{"a/x": 0}}, "interests": {Interests: 3}, "min likes": {MinLikes: 2},
		"a half-life that is the feed's": {HalfLifeDays: base.HalfLifeDays}, "a lookback that is the feed's": {LookbackDays: base.LookbackDays}} {
		if tune.likeKey(base) != same {
			t.Errorf("%s doesn't change how likes are read", name)
		}
	}
}

func TestTuningExcludes(t *testing.T) {
	cfg := PersonalConfigDefaults()
	now := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	post := func(ageHours float64, p float32) Post {
		return Post{IndexedAt: now.Add(-time.Duration(ageHours * float64(time.Hour))), TopPathP: p, Signals: map[string]float32{"substance": 0.9}}
	}
	// The window isn't checked unless the viewer set it: the feed's own pool decides what is in
	// range, and the clock has moved on since it was read.
	untuned := Tuning{}
	if untuned.Excludes(post(1000, 0.9), untuned.Config(cfg), now) {
		t.Error("an untuned viewer's window is the pool's business")
	}
	// The pool holds posts the model is less sure of than the feed's own setting, for viewers who
	// lower it: everyone else is shown only those at the feed's own.
	if !untuned.Excludes(post(1, 0.4), untuned.Config(cfg), now) || untuned.Excludes(post(1, 0.5), untuned.Config(cfg), now) {
		t.Error("an untuned viewer gets the feed's own min_topic_prob")
	}
	lower := Tuning{MinTopicProb: 0.35}
	if c := lower.Config(cfg); lower.Excludes(post(1, 0.4), c, now) || !lower.Excludes(post(1, 0.3), c, now) {
		t.Error("a viewer who lowered it to 0.35")
	}
	short := Tuning{WindowHours: 6}
	if c := short.Config(cfg); short.Excludes(post(5.9, 0.9), c, now) || !short.Excludes(post(6.1, 0.9), c, now) {
		t.Error("a window of six hours")
	}
	sure := Tuning{MinTopicProb: 0.8}
	if c := sure.Config(cfg); sure.Excludes(post(1, 0.8), c, now) || !sure.Excludes(post(1, 0.79), c, now) {
		t.Error("a topic probability of 0.8")
	}
	// What the model scored still leaves posts out.
	hide := Tuning{Signals: Rules{Min: map[string]float32{"substance": 0.95}}}
	if !hide.Excludes(post(1, 0.9), hide.Config(cfg), now) {
		t.Error("a minimum substance")
	}
}

func TestRankPoolWithBoostsOnToneAndSignals(t *testing.T) {
	at := time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)
	mk := func(name string, sig, tone map[string]float32) Post {
		return Post{URI: "at://did:plc:" + name + "/app.bsky.feed.post/1", DID: "did:plc:" + name, IndexedAt: at.Add(-time.Hour), Signals: sig, Tone: tone}
	}
	pool := map[string][]Post{"a/x": {
		mk("plain", map[string]float32{"substance": 0.5}, map[string]float32{"humorous": 0}),
		mk("funny", map[string]float32{"substance": 0.5}, map[string]float32{"humorous": 1}),
		mk("meaty", map[string]float32{"substance": 1}, map[string]float32{"humorous": 0}),
	}}
	r := DefaultRanking
	r.FreshEvery = 0
	order := func(p map[string][]Post) string {
		var names []string
		for _, x := range p["a/x"] {
			names = append(names, strings.TrimPrefix(x.DID, "did:plc:"))
		}
		return strings.Join(names, " ")
	}
	if got := order(RankPool(pool, r, at)); got != "meaty funny plain" && got != "meaty plain funny" {
		t.Errorf("no boosts: %s", got)
	}
	if got := order(RankPoolWith(pool, r, Rules{Weights: map[string]float64{"humorous": 5}}, Rules{}, nil, at)); got != "funny meaty plain" {
		t.Errorf("a boost on funny: %s", got)
	}
	if got := order(RankPoolWith(pool, r, Rules{}, Rules{Weights: map[string]float64{"substance": -1.5}}, nil, at)); got != "plain funny meaty" && got != "funny plain meaty" {
		t.Errorf("a boost against substance: %s", got)
	}
	if pool["a/x"][0].Score != 0 {
		t.Error("the feed's own pool was scored in place")
	}
}

func TestTuningHides(t *testing.T) {
	post := func(signals, tone map[string]float32) Post { return Post{Signals: signals, Tone: tone} }
	plain := post(map[string]float32{"substance": 0.6}, map[string]float32{"outraged": 0.2})
	if (Tuning{}).Hides(plain) {
		t.Error("no tuning hides nothing")
	}

	promo := Tuning{HidePromo: true}
	for name, p := range map[string]Post{
		"an ad":           post(map[string]float32{"ad": 0.31}, nil),
		"engagement bait": post(map[string]float32{"engagement_bait": 0.41}, nil),
		"spam":            post(map[string]float32{"spam": 0.11}, nil),
		"self-promotion":  post(map[string]float32{"self_promo": 0.51}, nil),
	} {
		if !promo.Hides(p) {
			t.Errorf("promo filter should hide %s", name)
		}
	}
	if promo.Hides(plain) || promo.Hides(post(map[string]float32{"ad": 0.3, "spam": 0.1}, nil)) || promo.Hides(Post{}) {
		t.Error("the promo filter hides only what is above its limits")
	}

	angry := Tuning{Tone: Rules{Max: map[string]float32{"outraged": 0.5}}}
	if !angry.Hides(post(nil, map[string]float32{"outraged": 0.51})) || angry.Hides(post(nil, map[string]float32{"outraged": 0.5})) || angry.Hides(Post{}) {
		t.Error("max outraged")
	}
	meaty := Tuning{Signals: Rules{Min: map[string]float32{"substance": 0.5}}}
	if !meaty.Hides(post(map[string]float32{"substance": 0.49}, nil)) || meaty.Hides(post(map[string]float32{"substance": 0.5}, nil)) {
		t.Error("min substance")
	}
	if !meaty.Hides(Post{}) {
		t.Error("a post with no substance score doesn't meet a minimum")
	}
}

func TestTuningJSON(t *testing.T) {
	b, err := json.Marshal(Tuning{})
	if err != nil || string(b) != "{}" {
		t.Errorf("the zero tuning is {}: %s %v", b, err)
	}
	in := Tuning{Topics: map[string]float64{"technology/ai": 2}, Freshness: FreshnessFresh, AuthorGap: ptr(0), HalfLifeDays: 3, HidePromo: true, ShowSeen: true,
		LookbackDays: 14, MinLikes: 3, Interests: 12, WindowHours: 12, MinTopicProb: 0.7, MaxServes: 1, ListSize: 80,
		Ranking: &RankingTuning{Gravity: ptr(0.0), FreshEvery: ptr(0), PromoPenalty: ptr(2.0), Like: ptr(1.5), Repost: ptr(0.0), Reply: ptr(3.0), Quote: ptr(4.0)},
		Tone:    Rules{Max: map[string]float32{"outraged": 0.4}, Min: map[string]float32{"informative": 0.1}, Weights: map[string]float64{"supportive": 1.5}},
		Signals: Rules{Min: map[string]float32{"substance": 0.2}, Max: map[string]float32{"spam": 0.1}, Weights: map[string]float64{"news": -1}}}
	b, _ = json.Marshal(in)
	var out Tuning
	dec := json.NewDecoder(strings.NewReader(string(b)))
	dec.DisallowUnknownFields() // what is written is what is read back, name for name
	if err := dec.Decode(&out); err != nil {
		t.Fatal(err)
	}
	if back, _ := json.Marshal(out); string(back) != string(b) {
		t.Errorf("round trip:\n%s\n%s", b, back)
	}
	// Zero values that mean something are kept.
	if out.AuthorGap == nil || *out.AuthorGap != 0 || out.Ranking.Gravity == nil || *out.Ranking.Gravity != 0 ||
		out.Ranking.FreshEvery == nil || *out.Ranking.FreshEvery != 0 || out.Ranking.Repost == nil || *out.Ranking.Repost != 0 {
		t.Errorf("a zero setting was lost: %s", b)
	}
	// And nothing is written for what is left alone.
	b, _ = json.Marshal(Tuning{Freshness: FreshnessFresh})
	if string(b) != `{"freshness":"fresh"}` {
		t.Errorf("%s", b)
	}
}
