package pipeline

import (
	"context"
	"fmt"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/haileyok/topic-feed/internal/chdb"
	"github.com/haileyok/topic-feed/internal/labelpolicy"
)

// RetryConfig controls the image retry worker.
type RetryConfig struct {
	Every       time.Duration   // how often to sweep for failures and run due retries
	Window      time.Duration   // how far back to look for failed images (feeds show 48h)
	Batch       int             // posts retried per round
	MaxAttempts int             // give up after this many failed retries
	Backoff     []time.Duration // wait before retry n+1 after n failed retries; the last repeats
	Workers     int             // posts retried at once
}

// DefaultRetry retries after about 2 min, 10 min, 30 min, 2 h, and 6 h, then gives up.
// Retries share the Describer's workers with live posts, so only a few run at once.
var DefaultRetry = RetryConfig{
	Every: 2 * time.Minute, Window: 48 * time.Hour, Batch: 200, MaxAttempts: 5, Workers: 4,
	Backoff: []time.Duration{2 * time.Minute, 10 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour},
}

// Queue statuses.
const (
	RetryPending = "pending"
	RetryFixed   = "fixed"
	RetryGaveUp  = "gave_up"
)

// retryable reports whether another try could find text for an image with this source.
// SourceNone (nothing usable in the image) and SourceBudget (retried tomorrow is too late
// for feeds) are final.
func retryable(source string) bool { return source == SourceError || source == SourceUnavailable }

// queueRow mirrors image_retry_queue.
type queueRow struct {
	URI           string    `ch:"uri"`
	DID           string    `ch:"did"`
	IndexedAt     time.Time `ch:"indexed_at"`
	Status        string    `ch:"status"`
	Attempts      uint8     `ch:"attempts"`
	NextAttemptAt time.Time `ch:"next_attempt_at"`
	LastError     string    `ch:"last_error"`
	QueuedAt      time.Time `ch:"queued_at"`
	UpdatedAt     time.Time `ch:"updated_at"`
}

// RetryImages runs the image retry worker until ctx is cancelled: every cfg.Every it adds
// recent posts with failed images to image_retry_queue, then retries the ones that are due.
// Posts stay in feeds meanwhile with their text-only classification; a successful retry
// re-classifies the post and replaces its post_pipeline row.
func (p *Pipeline) RetryImages(ctx context.Context, cfg RetryConfig) {
	p.Log.Info("image retries on", "every", cfg.Every, "window", cfg.Window, "max_attempts", cfg.MaxAttempts)
	for {
		if err := p.retryRound(ctx, cfg); err != nil && ctx.Err() == nil {
			metricErrors.WithLabelValues("retry").Inc()
			p.Log.Warn("image retry round failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(cfg.Every):
		}
	}
}

func (p *Pipeline) retryRound(ctx context.Context, cfg RetryConfig) error {
	// Sweep: every recent post with a retryable image failure that isn't queued yet. The
	// sweep (rather than queueing at write time) also catches failures from before the
	// worker existed and from a crash between writing a post and queueing it.
	if err := p.Conn.Exec(ctx, `
		INSERT INTO image_retry_queue (uri, did, indexed_at, status, attempts, next_attempt_at, last_error, queued_at, updated_at)
		SELECT uri, did, indexed_at, 'pending', 0, now64(3), '', now64(3), now64(3)
		FROM post_pipeline FINAL
		WHERE indexed_at > now64(6) - toIntervalSecond(?) AND feed_policy = ? AND model != ''
		  AND hasAny(image_text_sources, ['error', 'unavailable'])
		  AND uri NOT IN (SELECT uri FROM image_retry_queue WHERE indexed_at > now64(6) - toIntervalSecond(?))`,
		int64(cfg.Window.Seconds()), labelpolicy.OK, int64(cfg.Window.Seconds())); err != nil {
		return fmt.Errorf("sweep: %w", err)
	}
	var pending uint64
	if err := p.Conn.QueryRow(ctx, `SELECT count() FROM image_retry_queue FINAL WHERE status = 'pending'`).Scan(&pending); err == nil {
		metricRetryPending.Set(float64(pending))
	}

	var due []queueRow
	if err := p.Conn.Select(ctx, &due, `
		SELECT uri, did, indexed_at, status, attempts, next_attempt_at, last_error, queued_at, updated_at
		FROM image_retry_queue FINAL
		WHERE status = 'pending' AND next_attempt_at <= now64(3)
		ORDER BY next_attempt_at
		LIMIT ?`, cfg.Batch); err != nil {
		return fmt.Errorf("select due: %w", err)
	}
	if len(due) == 0 {
		return nil
	}
	uris := make([]string, len(due))
	since := due[0].IndexedAt
	for i, q := range due {
		uris[i] = q.URI
		if q.IndexedAt.Before(since) {
			since = q.IndexedAt
		}
	}
	var rs []rescoreRow
	if err := p.Conn.Select(ctx, &rs, `
		SELECT p.uri AS uri, p.did AS did, p.indexed_at AS indexed_at, p.text AS text, p.media_alts AS media_alts,
		       p.link_domain AS link_domain, p.link_title AS link_title, p.link_description AS link_description,
		       p.quote_text AS quote_text, p.tags AS tags, p.self_labels AS self_labels, p.media_kinds AS media_kinds,
		       p.media_cids AS media_cids, p.media_alt_texts AS media_alt_texts,
		       pp.processed_at AS processed_at, pp.feed_policy AS feed_policy, pp.labels AS labels,
		       pp.image_texts AS image_texts, pp.image_text_sources AS image_text_sources, pp.luna_cost_usd AS luna_cost_usd
		FROM (SELECT * FROM post_pipeline FINAL WHERE uri IN ? AND indexed_at >= ?) AS pp
		INNER JOIN (SELECT * FROM posts FINAL WHERE uri IN ? AND indexed_at >= ?) AS p ON p.uri = pp.uri`,
		uris, since, uris, since); err != nil {
		return fmt.Errorf("select posts: %w", err)
	}
	byURI := map[string]rescoreRow{}
	for _, r := range rs {
		byURI[r.URI] = r
	}

	type outcome struct {
		row     Row
		post    post
		changed bool   // image texts or sources changed: write a new post_pipeline row
		fixed   bool   // no retryable failure left
		paused  bool   // the only failures left were skipped while descriptions were paused
		err     string // first error message, for the queue
	}
	outs := make([]outcome, len(due))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, cfg.Workers))
	for i, q := range due {
		r, ok := byURI[q.URI]
		if !ok || r.FeedPolicy != labelpolicy.OK {
			outs[i] = outcome{fixed: true} // deleted, or no longer shown: nothing to retry
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, r rescoreRow) {
			defer func() { <-sem; wg.Done() }()
			row := Row{URI: r.URI, DID: r.DID, IndexedAt: r.IndexedAt, ProcessedAt: r.ProcessedAt.Add(time.Millisecond),
				FeedPolicy: r.FeedPolicy, Labels: nonNil(r.Labels), ImageTexts: slices.Clone(nonNil(r.ImageTexts)),
				ImageTextSources: slices.Clone(nonNil(r.ImageTextSources)), LLMCostUSD: r.LLMCostUSD,
				BroadProbs: map[string]float32{}, PathProbs: map[string]float32{}, Signals: map[string]float32{}, Tone: map[string]float32{}}
			o := outcome{row: row, post: r.post}
			for _, t := range retryTargets(r.post, r.ImageTextSources, p.Cfg.MaxMedia) {
				res := p.imageText(ctx, r.MediaKinds[t.attachment], r.DID, r.MediaCIDs[t.attachment])
				metricImages.WithLabelValues(res.Source).Inc()
				o.row.LLMCostUSD += res.Cost
				if res.Source != o.row.ImageTextSources[t.slot] || res.Text != o.row.ImageTexts[t.slot] {
					o.changed = true
				}
				o.row.ImageTexts[t.slot], o.row.ImageTextSources[t.slot] = res.Text, res.Source
				if res.Err != "" && o.err == "" {
					o.err = res.Err
				}
			}
			o.fixed = !slices.ContainsFunc(o.row.ImageTextSources, retryable)
			o.paused = !o.fixed && !slices.Contains(o.row.ImageTextSources, SourceError)
			if o.paused && o.err == "" {
				o.err = "descriptions paused"
			}
			outs[i] = o
		}(i, r)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}

	// Re-classify and write the posts whose image text changed.
	var posts []post
	var rows []Row
	for _, o := range outs {
		if o.changed {
			posts, rows = append(posts, o.post), append(rows, o.row)
		}
	}
	if len(rows) > 0 {
		if p.Classifier != nil {
			if err := p.classify(ctx, posts, rows); err != nil {
				return err
			}
		}
		wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		err := chdb.Insert(wctx, p.Conn, "post_pipeline", rows)
		cancel()
		if err != nil {
			return fmt.Errorf("write posts: %w", err)
		}
	}

	now := time.Now().UTC()
	updates := make([]queueRow, len(due))
	counts := map[string]int{}
	for i, q := range due {
		o := outs[i]
		q.Status, q.Attempts, q.NextAttemptAt = nextRetryState(q.Attempts, o.fixed, o.paused, now, cfg)
		q.LastError, q.UpdatedAt = truncateErr(o.err), now
		updates[i] = q
		result := q.Status
		if o.paused {
			result = "paused"
		} else if q.Status == RetryPending {
			result = "failed"
		}
		counts[result]++
		metricRetries.WithLabelValues(result).Inc()
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := chdb.Insert(wctx, p.Conn, "image_retry_queue", updates); err != nil {
		return fmt.Errorf("update queue: %w", err)
	}
	p.Log.Info("image retries", "tried", len(due), "rewritten", len(rows), "fixed", counts[RetryFixed],
		"failed", counts["failed"], "paused", counts["paused"], "gave_up", counts[RetryGaveUp], "pending", pending)
	return nil
}

// retryTarget is one attachment to retry: its index in the post's media and its slot in
// post_pipeline's image_texts (which only has entries for attachments without alt text).
type retryTarget struct{ attachment, slot int }

// retryTargets lists the attachments whose image step should be retried, matching them
// to image_texts slots the same way process() filled them: attachments in order, skipping
// those with alt text, at most maxMedia.
func retryTargets(ps post, sources []string, maxMedia int) []retryTarget {
	var out []retryTarget
	slot := 0
	for i := range ps.MediaCIDs {
		if slot >= len(sources) || slot >= maxMedia {
			break
		}
		if i < len(ps.MediaAltTexts) && strings.TrimSpace(ps.MediaAltTexts[i]) != "" {
			continue
		}
		if i < len(ps.MediaKinds) && retryable(sources[slot]) {
			out = append(out, retryTarget{attachment: i, slot: slot})
		}
		slot++
	}
	return out
}

// nextRetryState is a queue entry's state after a try. A try that only hit paused
// descriptions doesn't count as an attempt.
func nextRetryState(attempts uint8, fixed, paused bool, now time.Time, cfg RetryConfig) (string, uint8, time.Time) {
	if fixed {
		return RetryFixed, attempts, now
	}
	if !paused {
		attempts++
	}
	if int(attempts) >= cfg.MaxAttempts {
		return RetryGaveUp, attempts, now
	}
	wait := cfg.Backoff[min(int(attempts), len(cfg.Backoff)-1)]
	if paused {
		wait = cfg.Backoff[0] // pauses last a minute: come back soon
	}
	return RetryPending, attempts, now.Add(wait)
}

func truncateErr(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}
