package feedgen

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/topic-feed/internal/taxonomy"
)

const (
	maxSamples     = 8   // liked posts shown under each interest
	maxSampleRunes = 280 // of each liked post
)

// InterestsSource is what the interests view reads. *Store implements it.
type InterestsSource interface {
	LikeProfile(ctx context.Context, did string, since, now time.Time, halfLife time.Duration) (LikeData, error)
	LikedPosts(ctx context.Context, did string, since time.Time) ([]LikedPost, error)
	LikeCoverage(ctx context.Context, did string, since time.Time) (LikeCoverage, error)
}

var _ InterestsSource = (*Store)(nil)

// TopicName is how a subtopic, and the broad topic it belongs to, are named for people.
type TopicName struct{ Name, Broad string }

// TopicNames maps every broad topic and subtopic path to its names, from the taxonomy.
func TopicNames(tax *taxonomy.Taxonomy) map[string]TopicName {
	out := map[string]TopicName{}
	for _, b := range tax.Broad {
		out[b.ID] = TopicName{Name: b.Name, Broad: b.Name}
		for _, s := range b.Subtopics {
			out[b.ID+"/"+s.ID] = TopicName{Name: s.Name, Broad: b.Name}
		}
	}
	return out
}

// Sample is a post the viewer liked, shown as evidence for an interest.
type Sample struct {
	URL     string    `json:"url"` // the post in the Bluesky app
	Text    string    `json:"text"`
	LikedAt time.Time `json:"likedAt"`
	P       float64   `json:"p"` // how sure the model is of the topic
}

// Interest is one thing the viewer's likes say they are into.
type Interest struct {
	Path  string  `json:"path"`
	Name  string  `json:"name"`
	Broad string  `json:"broad"`
	Share float64 `json:"share"` // of all the viewer's interests from their likes alone, 0-1
	// Weight is how the viewer's tuning scales the topic: 0 muted, 1 as it is, up to 5.
	Weight float64 `json:"weight"`
	// TunedShare is the topic's share of the feed once the tuning is applied; 0 if muted.
	TunedShare float64 `json:"tunedShare"`
	// Added is a topic that isn't one of the interests from their likes alone: one they
	// turned on, or one that moved up because they muted others.
	Added bool `json:"added"`
	// Posts is how many of the posts they liked the model files under this topic. The share
	// comes from the model's probabilities for every post, so a topic can have a share
	// without posts filed under it.
	Posts   int      `json:"posts"`
	Samples []Sample `json:"samples"`
}

// InterestsResponse is what a viewer's likes say about them, in the terms the personal feed
// uses.
type InterestsResponse struct {
	DID      string `json:"did"`
	Handle   string `json:"handle"`
	Settings struct {
		LookbackDays int     `json:"lookbackDays"`
		HalfLifeDays float64 `json:"halfLifeDays"`
		MinLikes     int     `json:"minLikes"`
		Topics       int     `json:"topics"`
	} `json:"settings"`
	Coverage struct {
		Total          int `json:"total"`
		Classified     int `json:"classified"`
		Unclassified   int `json:"unclassified"`
		RepliesOrOther int `json:"repliesOrOther"`
		Unseen         int `json:"unseen"`
	} `json:"coverage"`
	// Personalized is whether there are enough classified likes for the feed to use these
	// interests; if not, the viewer's feed is an even mix of topics instead (unless they
	// have turned topics on).
	Personalized bool `json:"personalized"`
	// State is the kind of feed the viewer gets with their tuning: StatePersonal, or
	// StateGeneric, a mix of every topic.
	State        string     `json:"state"`
	Interests    []Interest `json:"interests"`
	OtherPosts   int        `json:"otherPosts"`   // liked posts filed under a topic that isn't a top interest
	UnclearPosts int        `json:"unclearPosts"` // liked posts the model couldn't place
	TookMs       int64      `json:"tookMs"`
}

// InterestsBuilder turns an account's likes into the interests the personal feed is built from.
type InterestsBuilder struct {
	Src   InterestsSource
	Cfg   PersonalConfig
	Names map[string]TopicName
	Now   func() time.Time // nil: time.Now
}

func (b *InterestsBuilder) now() time.Time {
	if b.Now != nil {
		return b.Now()
	}
	return time.Now()
}

// Build reads an account's likes. The three reads run together. t is the viewer's tuning (the
// zero Tuning for none): their likes are weighted by its half-life, and each interest
// reports how the tuning changes it. Topics the tuning adds are listed after the ones from
// the likes.
func (b *InterestsBuilder) Build(ctx context.Context, did, handle string, t Tuning) (*InterestsResponse, error) {
	start := b.now()
	cfg := t.Config(b.Cfg)
	since := start.AddDate(0, 0, -cfg.LookbackDays)
	halfLife := halfLifeDuration(cfg.HalfLifeDays)

	var (
		likes LikeData
		liked []LikedPost
		cov   LikeCoverage
		errs  [3]error
		wg    sync.WaitGroup
	)
	wg.Add(3)
	go func() { defer wg.Done(); likes, errs[0] = b.Src.LikeProfile(ctx, did, since, start, halfLife) }()
	go func() { defer wg.Done(); liked, errs[1] = b.Src.LikedPosts(ctx, did, since) }()
	go func() { defer wg.Done(); cov, errs[2] = b.Src.LikeCoverage(ctx, did, since) }()
	wg.Wait()
	if err := errors.Join(errs[:]...); err != nil {
		return nil, err
	}

	// What their likes say on their own, and what the feed uses once their tuning is applied
	// (with no pool here, a viewer without interests has none to show: their feed is a mix).
	prof := NewProfile(likes.Mass, likes.Posts, cfg.Topics, start)
	tuned, state := ProfileFor(cfg, t, likes.Mass, likes.Posts, nil, start)
	tunedShare := make(map[string]float64, len(tuned.Topics))
	for _, x := range tuned.Topics {
		tunedShare[x.Path] = x.Share
	}

	byPath := map[string][]LikedPost{}
	for _, lp := range liked { // newest first
		byPath[lp.TopPath] = append(byPath[lp.TopPath], lp)
	}
	resp := &InterestsResponse{DID: did, Handle: handle, Personalized: prof.Personalized(cfg.MinLikes), State: state, Interests: []Interest{}}
	resp.Settings.LookbackDays, resp.Settings.HalfLifeDays = cfg.LookbackDays, cfg.HalfLifeDays
	resp.Settings.MinLikes, resp.Settings.Topics = cfg.MinLikes, cfg.Topics
	resp.Coverage.Total, resp.Coverage.Classified = cov.Total, cov.Classified
	resp.Coverage.Unclassified, resp.Coverage.RepliesOrOther, resp.Coverage.Unseen = cov.Unclassified, cov.RepliesOrOther, cov.Unseen

	shown := map[string]bool{}
	add := func(path string, share float64, added bool) {
		shown[path] = true
		name := b.Names[path]
		if name.Name == "" {
			name = TopicName{Name: path}
		}
		weight, ok := t.Topics[path]
		if !ok {
			weight = 1
		}
		in := Interest{Path: path, Name: name.Name, Broad: name.Broad, Share: share, Weight: weight,
			TunedShare: tunedShare[path], Added: added, Posts: len(byPath[path]), Samples: []Sample{}}
		for _, lp := range byPath[path][:min(maxSamples, len(byPath[path]))] {
			in.Samples = append(in.Samples, toSample(lp))
		}
		resp.Interests = append(resp.Interests, in)
	}
	for _, x := range prof.Topics {
		add(x.Path, x.Share, false)
	}
	for _, x := range tuned.Topics { // strongest first; skips the ones above
		if !shown[x.Path] {
			add(x.Path, 0, true)
		}
	}
	for path, ps := range byPath {
		switch {
		case path == unclearTopic:
			resp.UnclearPosts += len(ps)
		case !shown[path]:
			resp.OtherPosts += len(ps)
		}
	}
	resp.TookMs = b.now().Sub(start).Milliseconds()
	return resp, nil
}

func toSample(lp LikedPost) Sample {
	return Sample{URL: postURL(lp.URI), Text: cleanText(lp.Text), LikedAt: lp.LikedAt.UTC(), P: float64(lp.TopP)}
}

// postURL is where a post is in the Bluesky app, or "" if uri is not a post's at:// URI.
func postURL(uri string) string {
	u, err := syntax.ParseATURI(uri)
	if err != nil {
		return ""
	}
	return "https://bsky.app/profile/" + u.Authority().String() + "/post/" + u.RecordKey().String()
}

// cleanText is a post's text on one line and no longer than a page shows.
func cleanText(s string) string {
	text := strings.Join(strings.Fields(s), " ")
	if r := []rune(text); len(r) > maxSampleRunes {
		text = string(r[:maxSampleRunes]) + "…"
	}
	return text
}
