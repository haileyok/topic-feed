package feedgen

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"
)

// fakeInterests is what an InterestsBuilder reads.
type fakeInterests struct {
	mu    sync.Mutex
	likes func(hl time.Duration) LikeData
	liked []LikedPost
	cov   LikeCoverage
	err   error
	hls   []time.Duration
	dids  []string // the account of each read of likes
}

func (f *fakeInterests) LikeProfile(_ context.Context, did string, _, _ time.Time, hl time.Duration) (LikeData, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	f.hls = append(f.hls, hl)
	f.dids = append(f.dids, did)
	return f.likes(hl), f.err
}

func (f *fakeInterests) reads() []string {
	f.mu.Lock()
	defer f.mu.Unlock()
	return append([]string(nil), f.dids...)
}

func (f *fakeInterests) failWith(err error) {
	f.mu.Lock()
	f.err = err
	f.mu.Unlock()
}

func (f *fakeInterests) LikedPosts(context.Context, string, time.Time) ([]LikedPost, error) {
	return f.liked, f.err
}

func (f *fakeInterests) LikeCoverage(context.Context, string, time.Time) (LikeCoverage, error) {
	return f.cov, f.err
}

func interestsBuilder(src *fakeInterests, tweak func(*PersonalConfig)) *InterestsBuilder {
	cfg := PersonalConfigDefaults()
	if tweak != nil {
		tweak(&cfg)
	}
	return &InterestsBuilder{Src: src, Cfg: cfg, Now: func() time.Time { return now },
		Names: map[string]TopicName{
			"a/x": {Name: "Alpha", Broad: "A"}, "b/x": {Name: "Beta", Broad: "B"}, "c/x": {Name: "Gamma", Broad: "C"},
			"d/x": {Name: "Delta", Broad: "D"}, "e/x": {Name: "Epsilon", Broad: "E"},
		}}
}

func likedIn(path string, n int) []LikedPost {
	out := make([]LikedPost, n)
	for i := range out {
		out[i] = LikedPost{URI: fmt.Sprintf("at://did:plc:%s/app.bsky.feed.post/%d", path[:1], i), Text: path, LikedAt: now, TopPath: path, TopP: 0.9}
	}
	return out
}

func interestByPath(r *InterestsResponse) map[string]Interest {
	out := map[string]Interest{}
	for _, in := range r.Interests {
		out[in.Path] = in
	}
	return out
}

func baseLikes(time.Duration) LikeData {
	return LikeData{Mass: map[string]float64{"a/x": 8, "b/x": 4, "c/x": 2, "d/x": 2, unclearTopic: 9}, Posts: 20}
}

func TestInterestsWithoutTuning(t *testing.T) {
	src := &fakeInterests{likes: baseLikes, liked: append(likedIn("a/x", 3), likedIn("e/x", 2)...)}
	r, err := interestsBuilder(src, nil).Build(context.Background(), "did:plc:v", "v.test", Tuning{})
	if err != nil {
		t.Fatal(err)
	}
	if !r.Personalized || r.State != StatePersonal || len(r.Interests) != 4 {
		t.Fatalf("personalized %v, state %q, %d interests", r.Personalized, r.State, len(r.Interests))
	}
	for _, in := range r.Interests {
		if in.Weight != 1 || in.Added || !near(in.TunedShare, in.Share) {
			t.Errorf("%s: weight %v, added %v, share %v, tuned %v: with no tuning nothing differs", in.Path, in.Weight, in.Added, in.Share, in.TunedShare)
		}
	}
	if in := r.Interests[0]; in.Path != "a/x" || !near(in.Share, .5) || in.Posts != 3 || len(in.Samples) != 3 || in.Name != "Alpha" {
		t.Errorf("the strongest interest: %+v", in)
	}
	if r.OtherPosts != 2 || r.Settings.HalfLifeDays != 7 {
		t.Errorf("other posts %d (the two of e/x), half-life %v", r.OtherPosts, r.Settings.HalfLifeDays)
	}
}

func TestInterestsShowWhatTheTuningChanges(t *testing.T) {
	src := &fakeInterests{likes: baseLikes, liked: append(likedIn("a/x", 3), likedIn("e/x", 2)...)}
	tune := Tuning{Topics: map[string]float64{
		"a/x": 0, // muted
		"b/x": 2, // turned up
		"e/x": 1, // added; they have liked two posts of it, which isn't enough to be an interest
		"d/x": 1,
	}}
	r, err := interestsBuilder(src, nil).Build(context.Background(), "did:plc:v", "v.test", tune)
	if err != nil {
		t.Fatal(err)
	}
	got := interestByPath(r)
	if len(r.Interests) != 5 {
		t.Fatalf("%d interests, want the four from likes and the added one", len(r.Interests))
	}
	// Likes alone are unchanged.
	if !near(got["a/x"].Share, .5) || !near(got["b/x"].Share, .25) {
		t.Errorf("natural shares: a %v, b %v", got["a/x"].Share, got["b/x"].Share)
	}
	// 8 -> muted, 4 -> 8, 2, 2, and e/x added at the median (3): total 15.
	if a := got["a/x"]; a.Weight != 0 || a.TunedShare != 0 || a.Added {
		t.Errorf("muted: %+v", a)
	}
	if b := got["b/x"]; b.Weight != 2 || !near(b.TunedShare, 8.0/15) {
		t.Errorf("turned up: weight %v, tuned share %v", b.Weight, b.TunedShare)
	}
	if c := got["c/x"]; c.Weight != 1 || !near(c.TunedShare, 2.0/15) {
		t.Errorf("untouched: weight %v, tuned share %v", c.Weight, c.TunedShare)
	}
	e := got["e/x"]
	if !e.Added || e.Share != 0 || e.Weight != 1 || !near(e.TunedShare, 3.0/15) || e.Posts != 2 || len(e.Samples) != 2 || e.Name != "Epsilon" {
		t.Errorf("added: %+v", e)
	}
	if r.Interests[4].Path != "e/x" {
		t.Errorf("added topics come after the interests from likes: %v", r.Interests)
	}
	if r.OtherPosts != 0 {
		t.Errorf("%d other posts: the two liked posts of e/x are shown under it", r.OtherPosts)
	}
	var total float64
	for _, in := range r.Interests {
		total += in.TunedShare
	}
	if !near(total, 1) {
		t.Errorf("tuned shares add up to %v", total)
	}
}

func TestInterestsMuteMakesRoomForTheNextOne(t *testing.T) {
	src := &fakeInterests{likes: func(time.Duration) LikeData {
		return LikeData{Mass: map[string]float64{"a/x": 10, "b/x": 5, "c/x": 3}, Posts: 20}
	}, liked: likedIn("c/x", 4)}
	r, err := interestsBuilder(src, func(c *PersonalConfig) { c.Topics = 2 }).Build(context.Background(), "did:plc:v", "v.test",
		Tuning{Topics: map[string]float64{"a/x": 0}})
	if err != nil {
		t.Fatal(err)
	}
	got := interestByPath(r)
	if len(r.Interests) != 3 {
		t.Fatalf("%d interests: %v", len(r.Interests), r.Interests)
	}
	if c := got["c/x"]; !c.Added || c.Share != 0 || !near(c.TunedShare, .375) || c.Posts != 4 {
		t.Errorf("the third interest moved up into the feed: %+v", c)
	}
	if !near(got["b/x"].TunedShare, .625) || got["a/x"].TunedShare != 0 {
		t.Errorf("%+v", got)
	}
}

func TestInterestsUseTheTuningsMemoryOfLikes(t *testing.T) {
	src := &fakeInterests{likes: func(hl time.Duration) LikeData {
		if hl <= 24*time.Hour {
			return LikeData{Mass: map[string]float64{"b/x": 9}, Posts: 20}
		}
		return LikeData{Mass: map[string]float64{"a/x": 9}, Posts: 20}
	}}
	b := interestsBuilder(src, nil)
	r, err := b.Build(context.Background(), "did:plc:v", "v.test", Tuning{HalfLifeDays: 1})
	if err != nil {
		t.Fatal(err)
	}
	if len(r.Interests) != 1 || r.Interests[0].Path != "b/x" || r.Settings.HalfLifeDays != 1 {
		t.Errorf("%+v, half-life %v", r.Interests, r.Settings.HalfLifeDays)
	}
	if len(src.hls) != 1 || src.hls[0] != 24*time.Hour {
		t.Errorf("likes read with %v", src.hls)
	}
	if r, _ = b.Build(context.Background(), "did:plc:v", "v.test", Tuning{}); r.Interests[0].Path != "a/x" || r.Settings.HalfLifeDays != 7 {
		t.Errorf("untuned: %+v", r.Interests)
	}
}

func TestInterestsForAViewerWhoHasLikedLittle(t *testing.T) {
	src := &fakeInterests{likes: func(time.Duration) LikeData {
		return LikeData{Mass: map[string]float64{"a/x": 2}, Posts: 2}
	}}
	b := interestsBuilder(src, nil)
	r, err := b.Build(context.Background(), "did:plc:v", "v.test", Tuning{})
	if err != nil {
		t.Fatal(err)
	}
	if r.Personalized || r.State != StateGeneric || len(r.Interests) != 1 || r.Interests[0].TunedShare != 0 {
		t.Errorf("untuned: personalized %v, state %q, %+v: the feed is a mix, so no interest has a tuned share", r.Personalized, r.State, r.Interests)
	}
	r, err = b.Build(context.Background(), "did:plc:v", "v.test", Tuning{Topics: map[string]float64{"e/x": 3, "d/x": 1}})
	if err != nil {
		t.Fatal(err)
	}
	got := interestByPath(r)
	if r.Personalized || r.State != StatePersonal || len(got) != 3 {
		t.Fatalf("with topics turned on: personalized %v, state %q, %v", r.Personalized, r.State, got)
	}
	if !near(got["e/x"].TunedShare, .75) || !near(got["d/x"].TunedShare, .25) || got["a/x"].TunedShare != 0 || !got["e/x"].Added || got["a/x"].Added {
		t.Errorf("the topics they chose are the feed: %+v", got)
	}
}

func TestInterestsReadErrors(t *testing.T) {
	src := &fakeInterests{likes: baseLikes, err: errors.New("clickhouse is down")}
	if _, err := interestsBuilder(src, nil).Build(context.Background(), "did:plc:v", "v.test", Tuning{}); err == nil {
		t.Error("a failed read was reported as done")
	}
}
