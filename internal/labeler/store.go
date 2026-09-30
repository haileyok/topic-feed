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

// Live selection modes for SelectLive.
const (
	LiveRandom    = "random"    // a random share of every classified post
	LiveUncertain = "uncertain" // posts the classifier was unsure about, or in its weakest topics
)

// weakTopics are the broad topics with the lowest classifier confidence on live posts
// (2026-09-29: humor 0.54, online_culture 0.59, lifestyle 0.61, personal_life 0.65).
var weakTopics = []string{"humor", "online_culture", "lifestyle", "personal_life"}

// SelectLive picks n posts the pipeline classified between from and to, with
// everything Jev should see: post fields, attachments, the text the pipeline found in
// images, and labels. Posts dropped by the label policy, deleted posts, and inactive
// authors are skipped.
//
// The n posts are fixed by a hash of the URI (seeded by mode), so a rerun picks the
// same posts and only labels the ones still missing. Posts already labeled under this
// label_config with another source are never picked, so the modes don't overlap.
func (s *Store) SelectLive(ctx context.Context, taxonomyVersion, labelConfig, source, mode string, from, to time.Time, n int) ([]Post, error) {
	cond := "1"
	switch mode {
	case LiveRandom:
	case LiveUncertain:
		cond = "(arrayMax(mapValues(pp.broad_probs)) < 0.5 OR pp.top_broad IN ?)"
	default:
		return nil, fmt.Errorf("unknown live mode %q", mode)
	}
	args := []any{}
	if mode == LiveUncertain {
		args = append(args, weakTopics)
	}
	args = append(args, from, to, taxonomyVersion, labelConfig, source, mode, n, taxonomyVersion, labelConfig)
	rows, err := s.Conn.Query(ctx, `
		SELECT uri, text, media_alts, link_domain, link_title, link_description, quote_text, tags,
		       media_kinds, image_texts, image_text_sources, labels
		FROM (
		    SELECT p.uri AS uri, p.text AS text, p.media_alts AS media_alts, p.link_domain AS link_domain,
		           p.link_title AS link_title, p.link_description AS link_description, p.quote_text AS quote_text,
		           p.tags AS tags, p.media_kinds AS media_kinds,
		           pp.image_texts AS image_texts, pp.image_text_sources AS image_text_sources, pp.labels AS labels
		    FROM post_pipeline AS pp FINAL
		    INNER JOIN (SELECT * FROM posts FINAL WHERE indexed_at >= ? - INTERVAL 1 MINUTE) AS p ON p.uri = pp.uri
		    WHERE pp.model != '' AND pp.feed_policy != 'drop' AND `+cond+`
		      AND pp.indexed_at >= ? AND pp.indexed_at < ?
		      AND pp.uri NOT IN (SELECT uri FROM jev_labels WHERE taxonomy_version = ? AND label_config = ? AND source != ?)
		      AND pp.uri NOT IN (SELECT uri FROM deletions WHERE collection = 'app.bsky.feed.post')
		      AND pp.did NOT IN (SELECT did FROM account_status FINAL WHERE active = 0)
		    ORDER BY cityHash64(pp.uri, ?)
		    LIMIT ?
		)
		WHERE uri NOT IN (SELECT uri FROM jev_labels WHERE taxonomy_version = ? AND label_config = ?)`,
		append([]any{from}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []Post
	for rows.Next() {
		var (
			uri, text, domain, title, desc, quote string
			alts, tags, kinds, imgTexts, imgSrcs  []string
			labels                                []string
		)
		if err := rows.Scan(&uri, &text, &alts, &domain, &title, &desc, &quote, &tags, &kinds, &imgTexts, &imgSrcs, &labels); err != nil {
			return nil, err
		}
		in := postdoc.Input{Text: text, MediaAlts: alts, LinkDomain: domain, LinkTitle: title,
			LinkDescription: desc, QuoteText: quote, Tags: tags, MediaKinds: kinds, Labels: labels}
		in.AddImageTexts(imgTexts, imgSrcs)
		out = append(out, Post{URI: uri, Doc: postdoc.New(in)})
	}
	return out, rows.Err()
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
