package feedgen

import (
	"context"
	"fmt"
	"time"
)

// LikedPost is a classified post a viewer liked or reposted: what their interests rest on.
type LikedPost struct {
	URI     string
	LikedAt time.Time
	Text    string
	TopPath string  // the subtopic the model files it under
	TopP    float32 // its probability
}

// LikedPosts returns the classified posts the viewer liked or reposted since the given time,
// most recent first. It is for showing a viewer what their interests are made of; the feed
// itself uses LikeProfile.
func (s *Store) LikedPosts(ctx context.Context, did string, since time.Time) ([]LikedPost, error) {
	react := []any{did, since, did, since}
	var args []any
	for range 3 { // the liked posts are named three times below
		args = append(args, react...)
	}
	var rows []struct {
		URI     string    `ch:"uri"`
		LikedAt time.Time `ch:"liked_at"`
		Text    string    `ch:"text"`
		TopPath string    `ch:"top_path"`
		TopP    float32   `ch:"top_p"`
	}
	if err := s.Conn.Select(ctx, &rows, `
		SELECT l.subject_uri AS uri, l.liked_at AS liked_at, p.text AS text, pp.top_path AS top_path, pp.top_path_p AS top_p
		FROM (SELECT subject_uri, max(indexed_at) AS liked_at FROM `+likesAndReposts+` GROUP BY subject_uri) AS l
		INNER JOIN (
			SELECT uri, top_path, top_path_p FROM post_pipeline FINAL
			WHERE model != '' AND uri IN (SELECT subject_uri FROM `+likesAndReposts+`)
		) AS pp ON l.subject_uri = pp.uri
		LEFT JOIN (
			SELECT uri, text FROM posts WHERE uri IN (SELECT subject_uri FROM `+likesAndReposts+`)
		) AS p ON l.subject_uri = p.uri
		ORDER BY liked_at DESC`, args...); err != nil {
		return nil, fmt.Errorf("select liked posts: %w", err)
	}
	out := make([]LikedPost, len(rows))
	for i, r := range rows {
		out[i] = LikedPost{URI: r.URI, LikedAt: r.LikedAt, Text: r.Text, TopPath: r.TopPath, TopP: r.TopP}
	}
	return out, nil
}

// LikeCoverage says what became of the posts a viewer liked or reposted: only top-level
// English posts are stored and classified, so many likes (replies, other languages) say
// nothing about the viewer's interests.
type LikeCoverage struct {
	Total int // distinct posts liked or reposted
	// Classified posts have topics, and are what interests are built from.
	Classified int
	// Unclassified are top-level English posts we stored but the classifier never scored:
	// mostly posts from before it was running.
	Unclassified int
	// RepliesOrOther are posts we saw but don't store or classify: replies and posts not in English.
	RepliesOrOther int
	// Unseen are posts we never saw at all: older than our data, or deleted.
	Unseen int
}

// LikeCoverage reads the coverage of the viewer's likes and reposts since the given time.
func (s *Store) LikeCoverage(ctx context.Context, did string, since time.Time) (LikeCoverage, error) {
	react := []any{did, since, did, since}
	var args []any
	for range 4 {
		args = append(args, react...)
	}
	var c struct {
		Total          uint64 `ch:"total"`
		Classified     uint64 `ch:"classified"`
		Unclassified   uint64 `ch:"unclassified"`
		RepliesOrOther uint64 `ch:"replies_or_other"`
		Unseen         uint64 `ch:"unseen"`
	}
	if err := s.Conn.QueryRow(ctx, `
		SELECT
			count() AS total,
			countIf(pp.uri != '' AND pp.model != '') AS classified,
			countIf(p.uri != '' AND NOT (pp.uri != '' AND pp.model != '')) AS unclassified,
			countIf(p.uri = '' AND pt.uri != '') AS replies_or_other,
			countIf(p.uri = '' AND pt.uri = '') AS unseen
		FROM (SELECT DISTINCT subject_uri FROM `+likesAndReposts+`) AS l
		LEFT JOIN (SELECT uri FROM posts WHERE uri IN (SELECT subject_uri FROM `+likesAndReposts+`)) AS p ON l.subject_uri = p.uri
		LEFT JOIN (SELECT uri, model FROM post_pipeline FINAL WHERE uri IN (SELECT subject_uri FROM `+likesAndReposts+`)) AS pp ON l.subject_uri = pp.uri
		LEFT JOIN (SELECT uri FROM post_texts WHERE uri IN (SELECT subject_uri FROM `+likesAndReposts+`)) AS pt ON l.subject_uri = pt.uri`,
		args...).ScanStruct(&c); err != nil {
		return LikeCoverage{}, fmt.Errorf("select like coverage: %w", err)
	}
	return LikeCoverage{Total: int(c.Total), Classified: int(c.Classified), Unclassified: int(c.Unclassified),
		RepliesOrOther: int(c.RepliesOrOther), Unseen: int(c.Unseen)}, nil
}
