package labeler

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/haileyok/topic-feed/internal/postdoc"
	"github.com/haileyok/topic-feed/internal/windows"
)

// Store reads posts to label and writes labels and request logs.
type Store struct {
	Conn driver.Conn
}

// SelectSpread picks up to n posts not yet labeled under (taxonomyVersion,
// labelConfig), spread evenly across the hours present in the data, skipping deleted
// posts and inactive authors. The order within each hour is a stable hash, so a
// rerun picks the same posts.
func (s *Store) SelectSpread(ctx context.Context, taxonomyVersion, labelConfig string, n int) ([]Post, error) {
	var hours uint64
	if err := s.Conn.QueryRow(ctx, "SELECT uniqExact(toStartOfHour(indexed_at)) FROM posts").Scan(&hours); err != nil {
		return nil, err
	}
	if hours == 0 {
		return nil, fmt.Errorf("posts table is empty")
	}
	perHour := (uint64(n) + hours - 1) / hours

	rows, err := s.Conn.Query(ctx, `
		SELECT uri, text, media_alts, link_domain, link_title, link_description, quote_text, tags
		FROM posts FINAL
		WHERE uri NOT IN (SELECT uri FROM jev_labels WHERE taxonomy_version = ? AND label_config = ?)
		  AND uri NOT IN (SELECT uri FROM deletions WHERE collection = 'app.bsky.feed.post')
		  AND did NOT IN (SELECT did FROM account_status FINAL WHERE active = 0)
		  AND `+hasTextOrAlt+`
		ORDER BY cityHash64(uri)
		LIMIT ? BY toStartOfHour(indexed_at)
		LIMIT ?`, taxonomyVersion, labelConfig, perHour, n)
	if err != nil {
		return nil, err
	}
	return scanPosts(rows)
}

// hasTextOrAlt skips posts with neither post text nor alt text (Hailey, 2026-09-28).
// A post with alt text but no text is kept; a link card or quote alone is not enough.
const hasTextOrAlt = `(trimBoth(text) != '' OR arrayExists(a -> trimBoth(a) != '', media_alts))`

// SelectWindows returns every post in the labeling windows that isn't yet labeled
// under (taxonomyVersion, labelConfig), in a stable hashed order (not time order, so
// a partial run still covers every window). Deleted posts, inactive authors, and posts
// with neither text nor alt text are skipped.
func (s *Store) SelectWindows(ctx context.Context, taxonomyVersion, labelConfig string, ws []windows.Window) ([]Post, error) {
	var conds []string
	args := []any{taxonomyVersion, labelConfig}
	for _, w := range ws {
		conds = append(conds, "(indexed_at >= ? AND indexed_at < ?)")
		args = append(args, w.Start, w.End)
	}
	rows, err := s.Conn.Query(ctx, `
		SELECT uri, text, media_alts, link_domain, link_title, link_description, quote_text, tags
		FROM posts FINAL
		WHERE uri NOT IN (SELECT uri FROM jev_labels WHERE taxonomy_version = ? AND label_config = ?)
		  AND (`+strings.Join(conds, " OR ")+`)
		  AND uri NOT IN (SELECT uri FROM deletions WHERE collection = 'app.bsky.feed.post')
		  AND did NOT IN (SELECT did FROM account_status FINAL WHERE active = 0)
		  AND `+hasTextOrAlt+`
		ORDER BY cityHash64(uri)`, args...)
	if err != nil {
		return nil, err
	}
	return scanPosts(rows)
}

func scanPosts(rows driver.Rows) ([]Post, error) {
	defer rows.Close()
	var out []Post
	for rows.Next() {
		var (
			uri, text, domain, title, desc, quote string
			alts, tags                            []string
		)
		if err := rows.Scan(&uri, &text, &alts, &domain, &title, &desc, &quote, &tags); err != nil {
			return nil, err
		}
		out = append(out, Post{URI: uri, Doc: postdoc.New(postdoc.Input{
			Text: text, MediaAlts: alts, LinkDomain: domain, LinkTitle: title,
			LinkDescription: desc, QuoteText: quote, Tags: tags,
		})})
	}
	return out, rows.Err()
}

// WriteLabels inserts labels into jev_labels.
func (s *Store) WriteLabels(ctx context.Context, taxonomyVersion, labelConfig, source string, batchSize int, labels []Label) error {
	if len(labels) == 0 {
		return nil
	}
	b, err := s.Conn.PrepareBatch(ctx, "INSERT INTO jev_labels")
	if err != nil {
		return err
	}
	for _, l := range labels {
		if err := b.Append(l.URI, taxonomyVersion, labelConfig, l.JevModel, source, uint16(batchSize), l.LabeledAt,
			l.BroadProbs, l.BroadConfidence, l.SubProbs, l.PathScores, l.Signals, l.RequestIDs); err != nil {
			_ = b.Abort()
			return err
		}
	}
	return b.Send()
}

// WriteRequests inserts request logs into jev_requests.
func (s *Store) WriteRequests(ctx context.Context, logs []RequestLog) error {
	if len(logs) == 0 {
		return nil
	}
	b, err := s.Conn.PrepareBatch(ctx, "INSERT INTO jev_requests")
	if err != nil {
		return err
	}
	for _, r := range logs {
		if err := b.Append(r.RequestID, r.TS, r.Pass, uint16(r.NPosts), uint16(r.NQuestions),
			uint32(r.InputTokens), uint32(r.LatencyMS), r.Status, r.Error); err != nil {
			_ = b.Abort()
			return err
		}
	}
	return b.Send()
}

// WithTimeout is a small helper for writes that must finish after a shutdown signal.
func WithTimeout(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
}
