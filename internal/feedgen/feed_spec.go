package feedgen

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
)

// Key identifies a feed inside the service: its rkey for the service owner's feeds (what the config
// file, the metrics and the inspector call them), and the owner's DID and the rkey for anyone
// else's, since two people can use the same rkey.
func (f Feed) Key() string {
	if f.Owner == "" {
		return f.Rkey
	}
	return f.Owner + "/" + f.Rkey
}

// lazy reports whether the feed is built only while somebody is asking for it. The service owner's
// feeds are always kept fresh; the thousands that people might make are not.
func (f Feed) lazy() bool { return f.Owner != "" }

// metricLabel names the feed in metrics. People's feeds share one label: one series per feed would
// grow without bound.
func (f Feed) metricLabel() string {
	if f.lazy() {
		return "user"
	}
	return f.Rkey
}

// FeedSpec is a feed as a person writes it, and as it is kept in the database (user_feeds.spec): what
// it takes, how it ranks, and how it is shown in Bluesky. It mirrors a feeds.yaml entry, without
// the feed's rkey (the key of the row) or anything of a personal feed.
type FeedSpec struct {
	DisplayName         string             `json:"display_name"`
	Description         string             `json:"description"`
	Paths               []string           `json:"paths"`
	Exclude             map[string]float32 `json:"exclude,omitempty"`
	MinProb             float32            `json:"min_prob"`
	AllowAdult          bool               `json:"allow_adult,omitempty"`
	Ranking             Ranking            `json:"ranking"`
	AcceptsInteractions bool               `json:"accepts_interactions"`
	Tone                Rules              `json:"tone"`
	Signals             Rules              `json:"signals"`
	MaxPosts            int                `json:"max_posts,omitempty"`
	MaxAgeMinutes       int                `json:"max_age_minutes,omitempty"`
	// TopicRules are tone and signal rules for particular topics (see TopicRules).
	TopicRules map[string]TopicRules `json:"topic_rules,omitempty"`
}

// UnmarshalJSON starts from the defaults a feeds.yaml entry starts from, so a spec only lists
// what differs: ranking fields it leaves out keep DefaultRanking's values.
func (s *FeedSpec) UnmarshalJSON(b []byte) error {
	type plain FeedSpec
	p := plain{Ranking: DefaultRanking, AcceptsInteractions: true}
	if err := json.Unmarshal(b, &p); err != nil {
		return err
	}
	*s = FeedSpec(p)
	return nil
}

// DecodeFeedSpec reads one spec from what a person sent. Unlike unmarshalling a stored spec, it
// refuses a field it doesn't know (a misspelt setting would otherwise be silently ignored) and
// anything after the spec but space.
func DecodeFeedSpec(r io.Reader) (FeedSpec, error) {
	type plain FeedSpec
	p := plain{Ranking: DefaultRanking, AcceptsInteractions: true}
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	if err := dec.Decode(&p); err != nil {
		return FeedSpec{}, err
	}
	switch tok, err := dec.Token(); {
	case errors.Is(err, io.EOF):
	case err != nil:
		return FeedSpec{}, err
	default:
		return FeedSpec{}, fmt.Errorf("unexpected %v after the feed", tok)
	}
	return FeedSpec(p), nil
}

// SpecOf is the spec of a feed from the config file. A personal feed has none.
func SpecOf(f Feed) (FeedSpec, error) {
	if f.Personal != nil {
		return FeedSpec{}, fmt.Errorf("feed %q is a personal feed: it is made from each viewer's likes, not from a spec", f.Rkey)
	}
	if f.Filtered != nil {
		return FeedSpec{}, fmt.Errorf("feed %q is a filtered feed: it is another feed's posts, not made from a spec", f.Rkey)
	}
	return FeedSpec{
		DisplayName: f.DisplayName, Description: f.Description, Paths: f.Paths, Exclude: f.Exclude, MinProb: f.MinProb,
		AllowAdult: f.AllowAdult, Ranking: f.Ranking, AcceptsInteractions: f.AcceptsInteractions, Tone: f.Tone,
		Signals: f.Signals, MaxPosts: f.MaxPosts, MaxAgeMinutes: f.MaxAgeMinutes, TopicRules: f.TopicRules,
	}, nil
}

// Feed is the feed this spec describes, as the record of owner's repo with the key rkey. owner is
// "" for the service owner's feeds (see Feed.Owner).
func (s FeedSpec) Feed(owner, rkey string) Feed {
	return Feed{
		Owner: owner, Rkey: rkey, DisplayName: s.DisplayName, Description: s.Description, Paths: s.Paths, Exclude: s.Exclude,
		MinProb: s.MinProb, AllowAdult: s.AllowAdult, Ranking: s.Ranking, AcceptsInteractions: s.AcceptsInteractions,
		Tone: s.Tone, Signals: s.Signals, MaxPosts: s.MaxPosts, MaxAgeMinutes: s.MaxAgeMinutes, TopicRules: s.TopicRules,
	}
}

// UserLimits bound what a person who doesn't own the service can ask of it: every feed is a query
// over a day of posts, run again and again while the feed is in use.
type UserLimits struct {
	MaxFeeds   int // feeds per person
	MaxPaths   int // topics a feed takes
	MaxExclude int // topics a feed leaves out
	MaxPosts   int // candidates per feed
}

// DefaultUserLimits are what people get.
var DefaultUserLimits = UserLimits{MaxFeeds: 5, MaxPaths: 40, MaxExclude: 60, MaxPosts: 3000}

// validateUser checks what is asked of a feed that isn't the service owner's, beyond what any feed
// must satisfy (Feed.validate): no adult posts, no personal feed, and the limits.
func (f Feed) validateUser(l UserLimits) error {
	switch {
	case f.Personal != nil:
		return fmt.Errorf("feed %q: personal feeds can't be made here", f.Rkey)
	case f.AllowAdult:
		return fmt.Errorf("feed %q: feeds that take adult posts can't be made here", f.Rkey)
	case len(f.Paths) > l.MaxPaths:
		return fmt.Errorf("feed %q: at most %d topics", f.Rkey, l.MaxPaths)
	case len(f.Exclude) > l.MaxExclude:
		return fmt.Errorf("feed %q: at most %d topics to leave out", f.Rkey, l.MaxExclude)
	case f.MaxPosts > l.MaxPosts:
		return fmt.Errorf("feed %q: max_posts is at most %d", f.Rkey, l.MaxPosts)
	}
	return nil
}
