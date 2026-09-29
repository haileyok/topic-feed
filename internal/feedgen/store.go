package feedgen

import (
	"context"
	"fmt"
	"slices"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/haileyok/topic-feed/internal/labelpolicy"
)

// Store builds feeds from ClickHouse.
type Store struct {
	Conn   driver.Conn
	Policy *labelpolicy.Policy
}

type candidate struct {
	URI        string    `ch:"uri"`
	DID        string    `ch:"did"`
	IndexedAt  time.Time `ch:"indexed_at"`
	FeedPolicy string    `ch:"feed_policy"`
	Labels     []string  `ch:"labels"`
	Score      float32   `ch:"score"`
}

// Removed counts candidates left out of a feed, by reason.
type Removed struct {
	Deleted, Inactive, Labeled int
}

// Build returns a feed's posts since the given time, newest first, at most limit.
func (s *Store) Build(ctx context.Context, f Feed, since time.Time, limit int) ([]Post, Removed, error) {
	policies := []string{labelpolicy.OK}
	if f.AllowAdult {
		policies = append(policies, labelpolicy.AdultOnly)
	}
	var cands []candidate
	// Posts the pipeline dropped are never classified (model = ''), so they can't match.
	err := s.Conn.Select(ctx, &cands, `
		SELECT uri, did, indexed_at, feed_policy, labels,
		       arrayMax(arrayMap(p -> path_probs[p], ?)) AS score
		FROM post_pipeline FINAL
		WHERE indexed_at >= ? AND model != '' AND feed_policy IN ? AND score >= ?
		ORDER BY indexed_at DESC, uri DESC
		LIMIT ?`, f.Paths, since, policies, f.MinProb, limit)
	if err != nil {
		return nil, Removed{}, fmt.Errorf("select candidates: %w", err)
	}
	var rm Removed
	if len(cands) == 0 {
		return []Post{}, rm, nil
	}

	uris := make([]string, 0, len(cands))
	dids := make([]string, 0, len(cands))
	seen := map[string]bool{}
	for _, c := range cands {
		uris = append(uris, c.URI)
		if !seen[c.DID] {
			seen[c.DID] = true
			dids = append(dids, c.DID)
		}
	}
	deleted, err := s.deleted(ctx, dids, uris)
	if err != nil {
		return nil, rm, err
	}
	inactive, err := s.inactive(ctx, dids)
	if err != nil {
		return nil, rm, err
	}
	labels, err := s.Policy.Current(ctx, s.Conn, append(slices.Clone(uris), dids...))
	if err != nil {
		return nil, rm, err
	}

	out := make([]Post, 0, len(cands))
	for _, c := range cands {
		switch {
		case deleted[c.URI]:
			rm.Deleted++
		case inactive[c.DID]:
			rm.Inactive++
		case !allowed(s.Policy, f, c, labels):
			rm.Labeled++
		default:
			out = append(out, Post{URI: c.URI, IndexedAt: c.IndexedAt, Score: c.Score})
		}
	}
	return out, rm, nil
}

// allowed re-applies the label policy with the labels the post had when processed plus
// the labels currently on the post and its author. Labels can only make a post stricter
// here: a label removed since processing still counts until the post is re-processed.
func allowed(p *labelpolicy.Policy, f Feed, c candidate, current map[string][]string) bool {
	all := slices.Concat(c.Labels, current[c.URI], current[c.DID])
	switch p.Decide(all) {
	case labelpolicy.OK:
		return true
	case labelpolicy.AdultOnly:
		return f.AllowAdult
	default:
		return false
	}
}

// deleted returns the URIs among uris whose posts were deleted.
func (s *Store) deleted(ctx context.Context, dids, uris []string) (map[string]bool, error) {
	var rows []struct {
		URI string `ch:"uri"`
	}
	// did IN uses the table's sort key (collection, did, rkey).
	err := s.Conn.Select(ctx, &rows, `
		SELECT DISTINCT uri FROM deletions
		WHERE collection = 'app.bsky.feed.post' AND did IN ? AND uri IN ?`, dids, uris)
	if err != nil {
		return nil, fmt.Errorf("select deletions: %w", err)
	}
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		out[r.URI] = true
	}
	return out, nil
}

// inactive returns the accounts among dids whose latest status is inactive (deactivated,
// suspended, taken down, deleted).
func (s *Store) inactive(ctx context.Context, dids []string) (map[string]bool, error) {
	var rows []struct {
		DID string `ch:"did"`
	}
	err := s.Conn.Select(ctx, &rows, `
		SELECT did FROM account_status
		WHERE did IN ?
		GROUP BY did
		HAVING argMax(active, indexed_at) = 0`, dids)
	if err != nil {
		return nil, fmt.Errorf("select account status: %w", err)
	}
	out := make(map[string]bool, len(rows))
	for _, r := range rows {
		out[r.DID] = true
	}
	return out, nil
}
