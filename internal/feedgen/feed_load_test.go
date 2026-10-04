package feedgen

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"
)

// fakeFeedStore is a FeedStore in memory.
type fakeFeedStore struct {
	feeds []StoredFeed
	err   error
}

func (f *fakeFeedStore) ListFeeds(context.Context) ([]StoredFeed, error) { return f.feeds, f.err }
func (f *fakeFeedStore) OwnerFeeds(_ context.Context, owner string) ([]StoredFeed, error) {
	var out []StoredFeed
	for _, s := range f.feeds {
		if s.Owner == owner {
			out = append(out, s)
		}
	}
	return out, f.err
}
func (f *fakeFeedStore) SaveFeed(_ context.Context, s StoredFeed) error {
	if f.err != nil {
		return f.err
	}
	for i, old := range f.feeds {
		if old.Owner == s.Owner && old.Rkey == s.Rkey {
			if s.CreatedAt.IsZero() {
				s.CreatedAt = old.CreatedAt
			}
			f.feeds[i] = s
			return nil
		}
	}
	if s.CreatedAt.IsZero() {
		s.CreatedAt = time.Now()
	}
	f.feeds = append(f.feeds, s)
	return nil
}
func (f *fakeFeedStore) DeleteFeed(_ context.Context, owner, rkey string) error {
	for i, s := range f.feeds {
		if s.Owner == owner && s.Rkey == rkey {
			f.feeds = append(f.feeds[:i], f.feeds[i+1:]...)
			break
		}
	}
	return f.err
}
func (f *fakeFeedStore) SeedFeeds(context.Context, string, []Feed) (int, error) { return 0, f.err }

func specOf(name string, paths ...string) FeedSpec {
	return FeedSpec{DisplayName: name, Paths: paths, MinProb: 0.5, Ranking: DefaultRanking, AcceptsInteractions: true}
}

func TestTheFeedsServedAreTheDatabasesAndThePersonalFeedOfTheFile(t *testing.T) {
	_, paths := realConfig(t)
	const owner = "did:plc:owner"
	store := &fakeFeedStore{feeds: []StoredFeed{
		{Owner: owner, Rkey: "ai", Spec: specOf("AI", "technology/ai")},
		{Owner: "did:plc:bob", Rkey: "cats", Spec: specOf("Cats", "animals_nature/cats")},
	}}
	// The file has a feed the database doesn't (taken down on the web) and a personal feed.
	cfg := &Config{Feeds: []Feed{ownerFeed("taken-down"), forYouFeed()}}
	got, err := LoadServedFeeds(context.Background(), store, cfg, owner, paths, quietLog)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, f := range got {
		keys = append(keys, f.Key())
	}
	if strings.Join(keys, " ") != "ai did:plc:bob/cats "+forYouFeed().Rkey {
		t.Errorf("served: %v", keys)
	}
	if got[0].Owner != "" || got[1].Owner != "did:plc:bob" {
		t.Errorf("the service owner's feeds have no owner, other people's do: %q %q", got[0].Owner, got[1].Owner)
	}
}

func TestAFeedThatCanNotBeServedIsLeftOutAndTheRestAreServed(t *testing.T) {
	_, paths := realConfig(t)
	const owner = "did:plc:owner"
	adult := specOf("Adult", "adult_content")
	adult.AllowAdult = true
	store := &fakeFeedStore{feeds: []StoredFeed{
		{Owner: owner, Rkey: "ok", Spec: specOf("OK", "technology/ai")},
		{Owner: owner, Rkey: "nonsense", Spec: specOf("Gone topic", "no_such/topic")},
		{Owner: "did:plc:bob", Rkey: "noname", Spec: specOf("", "technology/ai")},
		{Owner: "did:plc:bob", Rkey: "adult", Spec: adult},
		{Owner: owner, Rkey: "adult-mine", Spec: adult}, // the service owner may
		{Owner: "did:plc:carol", Rkey: "fine", Spec: specOf("Fine", "art")},
	}}
	got, err := LoadServedFeeds(context.Background(), store, &Config{}, owner, paths, quietLog)
	if err != nil {
		t.Fatal(err)
	}
	var keys []string
	for _, f := range got {
		keys = append(keys, f.Key())
	}
	if want := "ok adult-mine did:plc:carol/fine"; strings.Join(keys, " ") != want {
		t.Errorf("served %v, want %s", keys, want)
	}
}

func TestAnErrorReadingTheFeedsIsAnError(t *testing.T) {
	_, paths := realConfig(t)
	boom := errors.New("clickhouse is down")
	if _, err := LoadServedFeeds(context.Background(), &fakeFeedStore{err: boom}, &Config{}, "did:plc:owner", paths, quietLog); !errors.Is(err, boom) {
		t.Errorf("%v", err)
	}
}
