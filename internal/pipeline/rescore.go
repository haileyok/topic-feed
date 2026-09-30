package pipeline

import (
	"context"
	"fmt"
	"time"

	"github.com/haileyok/topic-feed/internal/chdb"
)

// rescoreRow is one post_pipeline row joined with its posts row.
type rescoreRow struct {
	post
	ProcessedAt      time.Time `ch:"processed_at"`
	FeedPolicy       string    `ch:"feed_policy"`
	Labels           []string  `ch:"labels"`
	ImageTexts       []string  `ch:"image_texts"`
	ImageTextSources []string  `ch:"image_text_sources"`
	LLMCostUSD       float64   `ch:"luna_cost_usd"`
}

// Rescore re-classifies posts that model oldModel classified before `before`, with the
// current classifier, reusing what the pipeline already stored for them (labels, label
// policy decision, text found in images): no images are fetched and no LLM is called.
//
// Each new row keeps the original processed_at plus 1ms, so it replaces the old row
// (post_pipeline is a ReplacingMergeTree on processed_at) without changing how late the
// post was processed. Only rows still on oldModel are picked, so a rerun resumes.
func (p *Pipeline) Rescore(ctx context.Context, oldModel string, before time.Time, window time.Duration) error {
	var first time.Time
	var n uint64
	if err := p.Conn.QueryRow(ctx, `SELECT min(indexed_at), count() FROM post_pipeline FINAL WHERE model = ? AND indexed_at < ?`,
		oldModel, before).Scan(&first, &n); err != nil {
		return err
	}
	if n == 0 {
		p.Log.Info("nothing to rescore", "model", oldModel)
		return nil
	}
	p.Log.Info("rescoring", "from_model", oldModel, "posts", n, "from", first, "before", before, "postdoc", p.PostdocVersion)
	done, start := 0, time.Now()
	for t := first.Truncate(window); t.Before(before); t = t.Add(window) {
		if err := ctx.Err(); err != nil {
			return err
		}
		end := t.Add(window)
		if end.After(before) {
			end = before
		}
		var rs []rescoreRow
		if err := p.Conn.Select(ctx, &rs, `
			SELECT p.uri AS uri, p.did AS did, p.indexed_at AS indexed_at, p.text AS text, p.media_alts AS media_alts,
			       p.link_domain AS link_domain, p.link_title AS link_title, p.link_description AS link_description,
			       p.quote_text AS quote_text, p.tags AS tags, p.self_labels AS self_labels, p.media_kinds AS media_kinds,
			       p.media_cids AS media_cids, p.media_alt_texts AS media_alt_texts,
			       pp.processed_at AS processed_at, pp.feed_policy AS feed_policy, pp.labels AS labels,
			       pp.image_texts AS image_texts, pp.image_text_sources AS image_text_sources, pp.luna_cost_usd AS luna_cost_usd
			FROM (SELECT * FROM post_pipeline FINAL WHERE indexed_at >= ? AND indexed_at < ? AND model = ?) AS pp
			INNER JOIN (SELECT * FROM posts FINAL WHERE indexed_at >= ? AND indexed_at < ?) AS p ON p.uri = pp.uri`,
			t, end, oldModel, t, end); err != nil {
			return fmt.Errorf("select %s: %w", t, err)
		}
		if len(rs) == 0 {
			continue
		}
		posts := make([]post, len(rs))
		rows := make([]Row, len(rs))
		for i, r := range rs {
			posts[i] = r.post
			rows[i] = Row{URI: r.URI, DID: r.DID, IndexedAt: r.IndexedAt, ProcessedAt: r.ProcessedAt.Add(time.Millisecond),
				FeedPolicy: r.FeedPolicy, Labels: nonNil(r.Labels), ImageTexts: nonNil(r.ImageTexts),
				ImageTextSources: nonNil(r.ImageTextSources), LLMCostUSD: r.LLMCostUSD,
				BroadProbs: map[string]float32{}, PathProbs: map[string]float32{}, Signals: map[string]float32{}, Tone: map[string]float32{}}
		}
		if err := p.classify(ctx, posts, rows); err != nil {
			return err
		}
		// Keep only rows the new model classified; anything it skipped keeps its old row.
		out := rows[:0]
		for _, r := range rows {
			if r.Model != "" && r.Model != oldModel {
				out = append(out, r)
			}
		}
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		err := chdb.Insert(wctx, p.Conn, "post_pipeline", out)
		cancel()
		if err != nil {
			return fmt.Errorf("insert %s: %w", t, err)
		}
		done += len(out)
		rate := float64(done) / time.Since(start).Seconds()
		p.Log.Info("rescored", "window", t.Format(time.RFC3339), "posts", len(out), "done", done, "of", n,
			"per_second", int(rate), "eta", (time.Duration(float64(int(n)-done)/max(rate, 1e-9)) * time.Second).Round(time.Minute).String())
	}
	p.Log.Info("rescore done", "posts", done)
	return nil
}
