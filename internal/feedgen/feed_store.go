package feedgen

import (
	"context"
	"encoding/json"
	"fmt"
	"slices"
	"time"

	"github.com/haileyok/topic-feed/internal/chdb"
)

// StoredFeed is a feed as kept in the database (schema/014_user_feeds.sql).
type StoredFeed struct {
	Owner     string // DID of the account whose repo holds the feed's record
	Rkey      string
	Spec      FeedSpec
	CreatedAt time.Time
	UpdatedAt time.Time
}

// Feed is the feed to serve. ownerDID is the DID of the service owner, whose feeds have no Owner
// (see Feed.Owner).
func (s StoredFeed) Feed(ownerDID string) Feed {
	owner := s.Owner
	if owner == ownerDID {
		owner = ""
	}
	return s.Spec.Feed(owner, s.Rkey)
}

// FeedStore reads and writes the feeds that are served. *Store implements it.
type FeedStore interface {
	// ListFeeds returns every feed that hasn't been deleted, oldest first.
	ListFeeds(ctx context.Context) ([]StoredFeed, error)
	// OwnerFeeds returns the feeds of one account, oldest first.
	OwnerFeeds(ctx context.Context, owner string) ([]StoredFeed, error)
	// SaveFeed stores a new version of a feed, creating it (or bringing back a deleted one).
	SaveFeed(ctx context.Context, f StoredFeed) error
	// DeleteFeed removes a feed. Deleting one that isn't there does nothing.
	DeleteFeed(ctx context.Context, owner, rkey string) error
	// SeedFeeds stores the feeds of the config file as owner's, except those that owner already has
	// a row for (even a deleted one: a feed taken down on the web stays down). It returns how many
	// it stored.
	SeedFeeds(ctx context.Context, owner string, feeds []Feed) (int, error)
}

var _ FeedStore = (*Store)(nil)

type feedRow struct {
	Owner     string    `ch:"owner_did"`
	Rkey      string    `ch:"rkey"`
	Spec      string    `ch:"spec"`
	Deleted   uint8     `ch:"deleted"`
	CreatedAt time.Time `ch:"created_at"`
	UpdatedAt time.Time `ch:"updated_at"`
}

// feedsSQL reads the newest version of every feed. Old versions are only dropped when parts merge,
// so this takes the newest rather than counting on a merge having happened. The WHERE is in
// the caller's hands (%s) and applies to every version, which is only right for the key. The
// aliases differ from the columns they are made of: ClickHouse would read a column's name, inside
// another aggregate, as the alias.
const feedsSQL = `
	SELECT owner_did, rkey,
	       argMax(spec, updated_at)    AS newest_spec,
	       argMax(deleted, updated_at) AS newest_deleted,
	       min(created_at)             AS first_created,
	       max(updated_at)             AS last_updated
	FROM user_feeds
	%s
	GROUP BY owner_did, rkey
	HAVING newest_deleted = 0
	ORDER BY first_created, owner_did, rkey`

// feedRead is a row of feedsSQL.
type feedRead struct {
	Owner   string    `ch:"owner_did"`
	Rkey    string    `ch:"rkey"`
	Spec    string    `ch:"newest_spec"`
	Deleted uint8     `ch:"newest_deleted"`
	Created time.Time `ch:"first_created"`
	Updated time.Time `ch:"last_updated"`
}

func (s *Store) ListFeeds(ctx context.Context) ([]StoredFeed, error) {
	return s.selectFeeds(ctx, fmt.Sprintf(feedsSQL, ""))
}

func (s *Store) OwnerFeeds(ctx context.Context, owner string) ([]StoredFeed, error) {
	return s.selectFeeds(ctx, fmt.Sprintf(feedsSQL, "WHERE owner_did = ?"), owner)
}

func (s *Store) selectFeeds(ctx context.Context, query string, args ...any) ([]StoredFeed, error) {
	var rows []feedRead
	if err := s.Conn.Select(ctx, &rows, query, args...); err != nil {
		return nil, fmt.Errorf("select feeds (is schema/014_user_feeds.sql applied?): %w", err)
	}
	out := make([]StoredFeed, 0, len(rows))
	for _, r := range rows {
		var spec FeedSpec
		if err := json.Unmarshal([]byte(r.Spec), &spec); err != nil {
			return nil, fmt.Errorf("the spec of feed %s/%s is not valid: %w", r.Owner, r.Rkey, err)
		}
		out = append(out, StoredFeed{Owner: r.Owner, Rkey: r.Rkey, Spec: spec, CreatedAt: r.Created, UpdatedAt: r.Updated})
	}
	return out, nil
}

func (s *Store) SaveFeed(ctx context.Context, f StoredFeed) error {
	return s.insertFeed(ctx, f, false)
}

func (s *Store) insertFeed(ctx context.Context, f StoredFeed, deleted bool) error {
	b, err := json.Marshal(f.Spec)
	if err != nil {
		return err
	}
	now := time.Now().UTC()
	created := f.CreatedAt
	if created.IsZero() {
		created = now
	}
	row := feedRow{Owner: f.Owner, Rkey: f.Rkey, Spec: string(b), CreatedAt: created, UpdatedAt: now}
	if deleted {
		row.Deleted = 1
	}
	return chdb.Insert(ctx, s.Conn, "user_feeds", []feedRow{row})
}

func (s *Store) DeleteFeed(ctx context.Context, owner, rkey string) error {
	existing, err := s.OwnerFeeds(ctx, owner)
	if err != nil {
		return err
	}
	i := slices.IndexFunc(existing, func(f StoredFeed) bool { return f.Rkey == rkey })
	if i < 0 {
		return nil
	}
	// The row that says "deleted" keeps what the feed was, for anyone looking back.
	return s.insertFeed(ctx, existing[i], true)
}

func (s *Store) SeedFeeds(ctx context.Context, owner string, feeds []Feed) (int, error) {
	var have []struct {
		Rkey string `ch:"rkey"`
	}
	if err := s.Conn.Select(ctx, &have, `SELECT rkey FROM user_feeds WHERE owner_did = ? GROUP BY rkey`, owner); err != nil {
		return 0, fmt.Errorf("select feeds (is schema/014_user_feeds.sql applied?): %w", err)
	}
	known := map[string]bool{}
	for _, h := range have {
		known[h.Rkey] = true
	}
	// Created a millisecond apart, in the file's order, which is the order they are listed in.
	base := time.Now().UTC().Truncate(time.Millisecond)
	n := 0
	for _, f := range feeds {
		if f.fromConfig() || known[f.Rkey] {
			continue
		}
		spec, err := SpecOf(f)
		if err != nil {
			return n, err
		}
		if err := s.SaveFeed(ctx, StoredFeed{Owner: owner, Rkey: f.Rkey, Spec: spec, CreatedAt: base.Add(time.Duration(n) * time.Millisecond)}); err != nil {
			return n, fmt.Errorf("seed feed %s: %w", f.Rkey, err)
		}
		n++
	}
	return n, nil
}
