package imagearchive

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Row types mirror schema/011_post_images.sql. The `ch` tags are the column names
// clickhouse-go uses for inserts and selects.

// ResolveRow is one row of post_image_resolve: what the AppView showed for a post,
// and the feed policy its labels give.
type ResolveRow struct {
	URI        string    `ch:"uri"`
	DID        string    `ch:"did"`
	Outcome    string    `ch:"outcome"` // found | no_images | gone
	NImages    uint8     `ch:"n_images"`
	Policy     string    `ch:"policy"` // ok | adult_only | drop; empty when the post has no media
	Labels     []string  `ch:"labels"`
	ResolvedAt time.Time `ch:"resolved_at"`
	UpdatedAt  time.Time `ch:"updated_at"`
}

// ImageRow is one row of post_images: one picture of a post.
type ImageRow struct {
	URI       string    `ch:"uri"`
	Idx       uint8     `ch:"idx"`
	Kind      string    `ch:"kind"` // image | video_thumb | link_card
	CID       string    `ch:"cid"`
	URL       string    `ch:"url"`
	Policy    string    `ch:"policy"`
	Status    string    `ch:"status"` // pending | ok | gone | error | bad_image | skipped_policy | purged
	Attempts  uint8     `ch:"attempts"`
	Sha256    string    `ch:"sha256"`
	Width     uint16    `ch:"width"`
	Height    uint16    `ch:"height"`
	Bytes     uint32    `ch:"bytes"`
	Error     string    `ch:"error"`
	FetchedAt time.Time `ch:"fetched_at"`
	UpdatedAt time.Time `ch:"updated_at"`
}

// Candidate is a labeled post the archive has not looked at yet.
type Candidate struct {
	URI       string `ch:"uri"`
	DID       string `ch:"did"`
	EmbedType string `ch:"embed_type"`
}

// OutcomeCount is posts per resolve outcome.
type OutcomeCount struct {
	Outcome string `ch:"outcome"`
	N       uint64 `ch:"n"`
}

// StatusCount is pictures per status, kind, and policy.
type StatusCount struct {
	Status string `ch:"status"`
	Kind   string `ch:"kind"`
	Policy string `ch:"policy"`
	N      uint64 `ch:"n"`
	Bytes  uint64 `ch:"bytes"`
}

// Stats is what the stats subcommand prints: posts by outcome, pictures by
// status/kind/policy, and the files those pictures add up to.
type Stats struct {
	Posts  []OutcomeCount
	Images []StatusCount
	Files  uint64 // distinct files among ok pictures
	Bytes  uint64 // bytes of ok pictures
}

// Store reads and writes the archive's tables.
type Store struct {
	Conn driver.Conn
}

// SelectUnresolved returns up to limit labeled posts (0 for all) that the archive has
// not looked at yet, skipping posts already deleted and posts by deactivated authors.
// The order is the hash of the URI, so the same posts come back first on every run and
// a stopped run leaves a clean prefix behind.
func (s *Store) SelectUnresolved(ctx context.Context, limit int) ([]Candidate, error) {
	q := `
		SELECT uri, did, embed_type
		FROM posts FINAL
		WHERE uri IN (SELECT DISTINCT uri FROM jev_labels)
		  AND uri NOT IN (SELECT uri FROM post_image_resolve FINAL)
		  AND uri NOT IN (SELECT uri FROM deletions WHERE collection = 'app.bsky.feed.post')
		  AND did NOT IN (SELECT did FROM account_status FINAL WHERE active = 0)
		ORDER BY cityHash64(uri)`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	var out []Candidate
	if err := s.Conn.Select(ctx, &out, q); err != nil {
		return nil, fmt.Errorf("select unresolved: %w", err)
	}
	return out, nil
}

// PendingImages returns up to limit pictures (0 for all) with one of statuses, ready to
// fetch. Error rows are only returned while they have fetches left to try.
func (s *Store) PendingImages(ctx context.Context, statuses []string, limit int) ([]ImageRow, error) {
	if len(statuses) == 0 {
		return nil, nil
	}
	q := `
		SELECT uri, idx, kind, cid, url, policy, status, attempts, sha256, width, height, bytes, error, fetched_at, updated_at
		FROM post_images FINAL
		WHERE status IN (?)
		  AND (status != 'error' OR attempts < 4)
		ORDER BY cityHash64(url)`
	if limit > 0 {
		q += fmt.Sprintf(" LIMIT %d", limit)
	}
	var out []ImageRow
	if err := s.Conn.Select(ctx, &out, q, statuses); err != nil {
		return nil, fmt.Errorf("select pending images: %w", err)
	}
	return out, nil
}

// Stats counts what the archive holds. Distinct files are counted over status ok rows;
// a picture posted by two posts is one file.
func (s *Store) Stats(ctx context.Context) (Stats, error) {
	var st Stats
	if err := s.Conn.Select(ctx, &st.Posts, `
		SELECT outcome, count() AS n
		FROM post_image_resolve FINAL
		GROUP BY outcome
		ORDER BY outcome`); err != nil {
		return st, fmt.Errorf("stats posts: %w", err)
	}
	if err := s.Conn.Select(ctx, &st.Images, `
		SELECT status, kind, policy, count() AS n, sum(bytes) AS bytes
		FROM post_images FINAL
		GROUP BY status, kind, policy
		ORDER BY status, kind, policy`); err != nil {
		return st, fmt.Errorf("stats images: %w", err)
	}
	var totals []struct {
		Files uint64 `ch:"files"`
		Bytes uint64 `ch:"bytes"`
	}
	if err := s.Conn.Select(ctx, &totals, `
		SELECT uniqExactIf(sha256, status = 'ok') AS files, sumIf(bytes, status = 'ok') AS bytes
		FROM post_images FINAL`); err != nil {
		return st, fmt.Errorf("stats totals: %w", err)
	}
	if len(totals) > 0 {
		st.Files, st.Bytes = totals[0].Files, totals[0].Bytes
	}
	return st, nil
}

// String renders Stats for the stats subcommand and the log.
func (s Stats) String() string {
	var b strings.Builder
	b.WriteString("posts by outcome:\n")
	for _, p := range s.Posts {
		fmt.Fprintf(&b, "  %-14s %d\n", p.Outcome, p.N)
	}
	b.WriteString("images by status/kind/policy:\n")
	for _, i := range s.Images {
		fmt.Fprintf(&b, "  %-14s %-12s %-10s %6d %14d bytes\n", i.Status, i.Kind, i.Policy, i.N, i.Bytes)
	}
	fmt.Fprintf(&b, "distinct files (ok): %d, %.1f GiB\n", s.Files, float64(s.Bytes)/(1<<30))
	return b.String()
}
