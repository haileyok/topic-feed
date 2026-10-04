package feedgen

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"

	"github.com/haileyok/topic-feed/internal/taxonomy"
)

func realConfig(t *testing.T) (*Config, map[string]bool) {
	t.Helper()
	tax, err := taxonomy.Load("../../taxonomy/v2.1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	cfg, err := LoadConfig("testdata/feeds.yaml", tax)
	if err != nil {
		t.Fatal(err)
	}
	return cfg, TaxonomyPaths(tax)
}

// Importing a feed of the config file into the database must not change what it does: a feed that
// goes through its stored form (JSON) and comes back is the same feed, for every feed of
// testdata/feeds.yaml (the topic feeds the config file had when they were imported).
func TestEveryFeedOfTheConfigFileSurvivesBeingStored(t *testing.T) {
	cfg, paths := realConfig(t)
	n := 0
	for _, f := range cfg.Feeds {
		if f.Personal != nil {
			if _, err := SpecOf(f); err == nil {
				t.Errorf("%s: a personal feed has no spec", f.Rkey)
			}
			continue
		}
		n++
		spec, err := SpecOf(f)
		if err != nil {
			t.Fatal(err)
		}
		b, err := json.Marshal(spec)
		if err != nil {
			t.Fatal(err)
		}
		var back FeedSpec
		if err := json.Unmarshal(b, &back); err != nil {
			t.Fatal(err)
		}
		got := back.Feed("", f.Rkey)
		if !reflect.DeepEqual(got, f) {
			t.Errorf("%s changed by being stored:\n  was  %+v\n  now  %+v", f.Rkey, f, got)
		}
		if err := got.validate(paths); err != nil {
			t.Errorf("%s: %v", f.Rkey, err)
		}
	}
	if n < 5 {
		t.Fatalf("only %d feeds in the config file: is this the right file?", n)
	}
}

func TestASpecStartsFromTheDefaultsOfAConfigEntry(t *testing.T) {
	var s FeedSpec
	if err := json.Unmarshal([]byte(`{"display_name":"Cats","paths":["animals_nature/cats"],"min_prob":0.5,"ranking":{"gravity":2.5}}`), &s); err != nil {
		t.Fatal(err)
	}
	want := DefaultRanking
	want.Gravity = 2.5
	if !reflect.DeepEqual(s.Ranking, want) {
		t.Errorf("ranking fields left out keep their defaults: %+v, want %+v", s.Ranking, want)
	}
	if !s.AcceptsInteractions {
		t.Error("a feed accepts interactions unless it says it doesn't")
	}
	if err := json.Unmarshal([]byte(`{"display_name":"Cats","accepts_interactions":false}`), &s); err != nil || s.AcceptsInteractions {
		t.Errorf("saying no is respected: %v, %+v", err, s.AcceptsInteractions)
	}
	if err := json.Unmarshal([]byte(`{"paths": 3}`), &s); err == nil {
		t.Error("a spec with the wrong kind of value is an error")
	}
}

func TestAFeedIsKnownByItsRkeyWhenItIsTheOwnersAndByOwnerAndRkeyWhenItIsNot(t *testing.T) {
	mine := Feed{Rkey: "ai"}
	theirs := Feed{Owner: "did:plc:bob", Rkey: "ai"}
	if mine.Key() != "ai" || theirs.Key() != "did:plc:bob/ai" || mine.Key() == theirs.Key() {
		t.Errorf("%q, %q", mine.Key(), theirs.Key())
	}
	if mine.lazy() || !theirs.lazy() {
		t.Errorf("the owner's feeds are kept fresh, other people's are built when asked for: %v %v", mine.lazy(), theirs.lazy())
	}
	if mine.metricLabel() != "ai" || theirs.metricLabel() != "user" || (Feed{Owner: "did:plc:carol", Rkey: "x"}).metricLabel() != "user" {
		t.Error("metrics name the owner's feeds, and put everyone else's under one label")
	}
}

func TestStoredFeedsOfTheServiceOwnerHaveNoOwner(t *testing.T) {
	spec := FeedSpec{DisplayName: "AI", Paths: []string{"technology/ai"}, MinProb: 0.5, Ranking: DefaultRanking}
	if f := (StoredFeed{Owner: "did:plc:owner", Rkey: "ai", Spec: spec}).Feed("did:plc:owner"); f.Owner != "" || f.Key() != "ai" {
		t.Errorf("%+v", f)
	}
	if f := (StoredFeed{Owner: "did:plc:bob", Rkey: "ai", Spec: spec}).Feed("did:plc:owner"); f.Owner != "did:plc:bob" || f.Key() != "did:plc:bob/ai" {
		t.Errorf("%+v", f)
	}
}

func TestWhatAPersonMayAskOfTheService(t *testing.T) {
	_, paths := realConfig(t)
	base := func() Feed {
		return Feed{Owner: "did:plc:bob", Rkey: "cats", DisplayName: "Cats", Paths: []string{"animals_nature/cats"}, MinProb: 0.5,
			Ranking: DefaultRanking, AcceptsInteractions: true}
	}
	l := UserLimits{MaxFeeds: 5, MaxPaths: 2, MaxExclude: 1, MaxPosts: 100}
	ok := base()
	if err := ok.validate(paths); err != nil {
		t.Fatal(err)
	}
	if err := ok.validateUser(l); err != nil {
		t.Errorf("an ordinary feed: %v", err)
	}
	cases := map[string]func(*Feed){
		"adult posts":      func(f *Feed) { f.AllowAdult = true },
		"a personal feed":  func(f *Feed) { f.Personal = &PersonalConfig{} },
		"too many topics":  func(f *Feed) { f.Paths = []string{"animals_nature/cats", "animals_nature/dogs", "art"} },
		"too many exclude": func(f *Feed) { f.Exclude = map[string]float32{"art": 0.3, "us_politics": 0.3} },
		"too many posts":   func(f *Feed) { f.MaxPosts = 101 },
	}
	for name, mutate := range cases {
		f := base()
		mutate(&f)
		if err := f.validateUser(l); err == nil {
			t.Errorf("%s: allowed", name)
		}
	}
	// What is on the limit is fine.
	f := base()
	f.Paths = []string{"animals_nature/cats", "animals_nature/dogs"}
	f.Exclude = map[string]float32{"art": 0.3}
	f.MaxPosts = 100
	if err := f.validateUser(l); err != nil {
		t.Errorf("at the limits: %v", err)
	}
	if DefaultUserLimits.MaxFeeds < 1 || DefaultUserLimits.MaxPaths < 1 || DefaultUserLimits.MaxPosts > 20000 {
		t.Errorf("%+v", DefaultUserLimits)
	}
}

// The checks of one feed are the checks of the config file: an entry that Validate refuses is refused
// the same way when one feed is checked.
func TestOneFeedIsCheckedAsTheConfigFileChecksIt(t *testing.T) {
	_, paths := realConfig(t)
	bad := []Feed{
		{Rkey: "Bad Key", DisplayName: "x", Paths: []string{"art"}, MinProb: 0.5},
		{Rkey: "ok", DisplayName: "", Paths: []string{"art"}, MinProb: 0.5},
		{Rkey: "ok", DisplayName: strings.Repeat("x", 25), Paths: []string{"art"}, MinProb: 0.5},
		{Rkey: "ok", DisplayName: "x", Description: strings.Repeat("x", 301), Paths: []string{"art"}, MinProb: 0.5},
		{Rkey: "ok", DisplayName: "x", MinProb: 0.5},
		{Rkey: "ok", DisplayName: "x", Paths: []string{"nonsense/topic"}, MinProb: 0.5},
		{Rkey: "ok", DisplayName: "x", Paths: []string{"*", "art"}, MinProb: 0.5},
		{Rkey: "ok", DisplayName: "x", Paths: []string{"art"}, MinProb: 0},
		{Rkey: "ok", DisplayName: "x", Paths: []string{"art"}, MinProb: 1.5},
		{Rkey: "ok", DisplayName: "x", Paths: []string{"art"}, MinProb: 0.5, Exclude: map[string]float32{"nonsense": 0.3}},
		{Rkey: "ok", DisplayName: "x", Paths: []string{"art"}, MinProb: 0.5, MaxPosts: 20001},
		{Rkey: "ok", DisplayName: "x", Paths: []string{"art"}, MinProb: 0.5, Ranking: Ranking{FreshEvery: 1}},
		{Rkey: "ok", DisplayName: "x", Paths: []string{"art"}, MinProb: 0.5, Tone: Rules{Max: map[string]float32{"furious": 0.3}}},
	}
	for i, f := range bad {
		if err := f.validate(paths); err == nil {
			t.Errorf("%d: %+v accepted", i, f)
		}
		if err := (&Config{Feeds: []Feed{f}}).Validate(paths); err == nil {
			t.Errorf("%d: the config file accepts what one feed doesn't", i)
		}
	}
	dup := Feed{Rkey: "ok", DisplayName: "x", Paths: []string{"art"}, MinProb: 0.5, Ranking: DefaultRanking}
	if err := (&Config{Feeds: []Feed{dup, dup}}).Validate(paths); err == nil || !strings.Contains(err.Error(), "duplicate") {
		t.Errorf("duplicate rkeys in the file: %v", err)
	}
}
