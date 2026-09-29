package feedgen

import (
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/haileyok/topic-feed/internal/labelpolicy"
)

// maxQuerySize allows IN lists of up to 20,000 post URIs (the max_posts limit).
const maxQuerySize = 4 << 20

// Store builds feeds from ClickHouse.
type Store struct {
	Conn   driver.Conn
	Policy *labelpolicy.Policy
}

type candidate struct {
	URI        string             `ch:"uri"`
	DID        string             `ch:"did"`
	IndexedAt  time.Time          `ch:"indexed_at"`
	FeedPolicy string             `ch:"feed_policy"`
	Labels     []string           `ch:"labels"`
	Score      float32            `ch:"score"`
	Signals    map[string]float32 `ch:"signals"`
	TopPath    string             `ch:"top_path"`
	TopPathP   float32            `ch:"top_path_p"`
	TopPaths   []string           `ch:"top_paths"`
	TopPs      []float32          `ch:"top_ps"`
	Tone       map[string]float32 `ch:"tone"`
}

// Removed counts candidates left out of a feed, by reason.
type Removed struct {
	Deleted, Inactive, Labeled int
}

// Build returns a feed's candidate posts since the given time with their engagement,
// newest first, at most limit. Ranking happens in the caller.
func (s *Store) Build(ctx context.Context, f Feed, since time.Time, limit int) ([]Post, Removed, error) {
	// The lookups below pass every candidate's URI; with max_posts at 10,000 that is
	// ~700 KB of query text, past ClickHouse's 256 KB default.
	ctx = clickhouse.Context(ctx, clickhouse.WithSettings(clickhouse.Settings{"max_query_size": maxQuerySize}))
	policies := []string{labelpolicy.OK}
	if f.AllowAdult {
		policies = append(policies, labelpolicy.AdultOnly)
	}
	// The feed's match: the best of its subtopic and broad-topic probabilities, or 1 for
	// any topic.
	var scoreExpr string
	var args []any
	if f.anyTopic() {
		scoreExpr = "toFloat32(1)"
	} else {
		subs, broads := split(f.Paths)
		scoreExpr = "greatest(arrayMax(arrayMap(p -> path_probs[p], ?)), arrayMax(arrayMap(b -> broad_probs[b], ?)))"
		args = append(args, subs, broads)
	}
	args = append(args, since, policies, f.MinProb)
	// Exclusions and tone/signal cutoffs, as conditions on the stored scores, so the limit
	// counts only posts that pass. Names are checked when the config loads, and passed as
	// parameters.
	var where strings.Builder
	for _, p := range slices.Sorted(maps.Keys(f.Exclude)) {
		if isBroad(p) {
			where.WriteString(" AND broad_probs[?] <= ?")
		} else {
			where.WriteString(" AND path_probs[?] <= ?")
		}
		args = append(args, p, f.Exclude[p])
	}
	for _, r := range []struct {
		col   string
		rules Rules
	}{{"tone", f.Tone}, {"signals", f.Signals}} {
		for _, name := range slices.Sorted(maps.Keys(r.rules.Max)) {
			where.WriteString(" AND " + r.col + "[?] <= ?")
			args = append(args, name, r.rules.Max[name])
		}
		for _, name := range slices.Sorted(maps.Keys(r.rules.Min)) {
			where.WriteString(" AND " + r.col + "[?] >= ?")
			args = append(args, name, r.rules.Min[name])
		}
	}
	args = append(args, limit)
	var cands []candidate
	// Posts the pipeline dropped are never classified (model = ''), so they can't match.
	err := s.Conn.Select(ctx, &cands, `
		SELECT uri, did, indexed_at, feed_policy, labels, `+scoreExpr+` AS score,
		       signals, top_path, top_path_p,
		       arrayMap(kv -> kv.1, arraySlice(arraySort(kv -> -kv.2, arrayZip(mapKeys(path_probs), mapValues(path_probs))), 1, 3)) AS top_paths,
		       arrayMap(kv -> kv.2, arraySlice(arraySort(kv -> -kv.2, arrayZip(mapKeys(path_probs), mapValues(path_probs))), 1, 3)) AS top_ps,
		       tone
		FROM post_pipeline FINAL
		WHERE indexed_at >= ? AND model != '' AND feed_policy IN ? AND score >= ?`+where.String()+`
		ORDER BY indexed_at DESC, uri DESC
		LIMIT ?`, args...)
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
			out = append(out, Post{URI: c.URI, DID: c.DID, IndexedAt: c.IndexedAt, Match: c.Score,
				Signals: c.Signals,
				TopPath: c.TopPath, TopPathP: c.TopPathP, TopPaths: c.TopPaths, TopPs: c.TopPs, Tone: c.Tone})
		}
	}
	if err := s.engagement(ctx, out); err != nil {
		return nil, rm, err
	}
	return out, rm, nil
}

// engagement fills in each post's like, repost, reply, and quote counts.
func (s *Store) engagement(ctx context.Context, posts []Post) error {
	if len(posts) == 0 {
		return nil
	}
	idx := make(map[string]int, len(posts))
	uris := make([]string, len(posts))
	for i, p := range posts {
		idx[p.URI] = i
		uris[i] = p.URI
	}
	var likes []struct {
		URI string `ch:"subject_uri"`
		N   uint64 `ch:"n"`
	}
	if err := s.Conn.Select(ctx, &likes, `
		SELECT subject_uri, sum(likes) AS n FROM like_counts_hourly
		WHERE subject_uri IN ? GROUP BY subject_uri`, uris); err != nil {
		return fmt.Errorf("select likes: %w", err)
	}
	for _, r := range likes {
		posts[idx[r.URI]].Likes = r.N
	}
	var other []struct {
		URI  string `ch:"subject_uri"`
		Kind string `ch:"kind"`
		N    uint64 `ch:"n"`
	}
	if err := s.Conn.Select(ctx, &other, `
		SELECT subject_uri, kind, sum(n) AS n FROM engagement_hourly
		WHERE subject_uri IN ? GROUP BY subject_uri, kind`, uris); err != nil {
		return fmt.Errorf("select engagement: %w", err)
	}
	for _, r := range other {
		p := &posts[idx[r.URI]]
		switch r.Kind {
		case "repost":
			p.Reposts = r.N
		case "reply":
			p.Replies = r.N
		case "quote":
			p.Quotes = r.N
		}
	}
	return nil
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

// Volumes returns recent posts per hour for every broad topic and subtopic, counting a
// post for each topic or subtopic it scores at least 0.5 on (over the last 3 hours).
func (s *Store) Volumes(ctx context.Context) (map[string]float64, error) {
	var rows []struct {
		Key string `ch:"key"`
		N   uint64 `ch:"n"`
	}
	err := s.Conn.Select(ctx, &rows, `
		SELECT key, count() AS n FROM (
			SELECT arrayConcat(
				arrayFilter(k -> path_probs[k] >= 0.5, mapKeys(path_probs)),
				arrayFilter(k -> broad_probs[k] >= 0.5, mapKeys(broad_probs))) AS keys
			FROM post_pipeline FINAL
			WHERE indexed_at > now() - INTERVAL 3 HOUR AND model != '' AND feed_policy = 'ok'
		) ARRAY JOIN keys AS key
		GROUP BY key`)
	if err != nil {
		return nil, fmt.Errorf("select volumes: %w", err)
	}
	out := make(map[string]float64, len(rows))
	for _, r := range rows {
		out[r.Key] = float64(r.N) / 3
	}
	return out, nil
}
