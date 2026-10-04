package pipeline

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/haileyok/topic-feed/internal/chdb"
	"github.com/haileyok/topic-feed/internal/labelpolicy"
)

// RetryConfig controls the picture retry worker.
type RetryConfig struct {
	Every       time.Duration   // how often to sweep for failures and run due retries
	Window      time.Duration   // how far back to look for posts missing pictures (feeds show 24h)
	Batch       int             // posts retried per round
	MaxAttempts int             // give up after this many failed retries
	Backoff     []time.Duration // wait before retry n+1 after n failed retries; the last repeats
	Workers     int             // posts retried at once
}

// DefaultRetry retries after about 2 min, 10 min, 30 min, 2 h, and 6 h, then gives up.
var DefaultRetry = RetryConfig{
	Every: 2 * time.Minute, Window: 24 * time.Hour, Batch: 200, MaxAttempts: 5, Workers: 4,
	Backoff: []time.Duration{2 * time.Minute, 10 * time.Minute, 30 * time.Minute, 2 * time.Hour, 6 * time.Hour},
}

// Queue statuses.
const (
	RetryPending = "pending"
	RetryFixed   = "fixed"
	RetryGaveUp  = "gave_up"
)

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

// RetryPictures runs the picture retry worker until ctx is cancelled: every cfg.Every it adds
// recent posts the model saw fewer pictures of than it should have to image_retry_queue, then
// downloads the missing pictures for the ones that are due. Posts stay in feeds meanwhile with
// the classification they got without those pictures; a successful retry re-classifies the post
// and replaces its post_pipeline row.
func (p *Pipeline) RetryPictures(ctx context.Context, cfg RetryConfig) {
	p.Log.Info("picture retries on", "every", cfg.Every, "window", cfg.Window, "max_attempts", cfg.MaxAttempts)
	for {
		if err := p.retryRound(ctx, cfg); err != nil && ctx.Err() == nil {
			metricErrors.WithLabelValues("retry").Inc()
			p.Log.Warn("picture retry round failed", "err", err)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(cfg.Every):
		}
	}
}

func (p *Pipeline) retryRound(ctx context.Context, cfg RetryConfig) error {
	// Sweep: every recent post whose pictures didn't all download and that isn't queued yet. The
	// sweep (rather than queueing at write time) also catches a crash between writing a post and
	// queueing it.
	if err := p.Conn.Exec(ctx, `
		INSERT INTO image_retry_queue (uri, did, indexed_at, status, attempts, next_attempt_at, last_error, queued_at, updated_at)
		SELECT uri, did, indexed_at, 'pending', 0, now64(3), '', now64(3), now64(3)
		FROM post_pipeline FINAL
		WHERE indexed_at > now64(6) - toIntervalSecond(?) AND feed_policy != ? AND pictures_used < pictures_wanted
		  AND uri NOT IN (SELECT uri FROM image_retry_queue WHERE indexed_at > now64(6) - toIntervalSecond(?))`,
		int64(cfg.Window.Seconds()), labelpolicy.Drop, int64(cfg.Window.Seconds())); err != nil {
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
	if err := p.Conn.Select(ctx, &rs, rescoreSelect+`
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
		pics    [][]byte
		changed bool // more pictures than before: write a new post_pipeline row
		fixed   bool // every picture downloaded, or nothing left to retry
		err     string
	}
	outs := make([]outcome, len(due))
	var wg sync.WaitGroup
	sem := make(chan struct{}, max(1, cfg.Workers))
	for i, q := range due {
		r, ok := byURI[q.URI]
		if !ok || r.FeedPolicy == labelpolicy.Drop {
			outs[i] = outcome{fixed: true} // deleted, or no longer shown: nothing to retry
			continue
		}
		wg.Add(1)
		sem <- struct{}{}
		go func(i int, r rescoreRow) {
			defer func() { <-sem; wg.Done() }()
			row := r.row()
			refs := pictureRefs(r.post, p.Cfg.MaxPictures)
			pics, failed := p.fetchPictures(ctx, r.DID, refs)
			o := outcome{row: row, post: r.post, pics: pics}
			o.row.PicturesWanted, o.row.PicturesUsed = uint8(len(refs)), uint8(len(pics))
			o.changed = len(pics) > int(r.PicturesUsed)
			o.fixed = failed == 0
			if failed > 0 {
				o.err = fmt.Sprintf("%d of %d pictures did not download", failed, len(refs))
			}
			outs[i] = o
		}(i, r)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return err
	}

	// Re-classify and write the posts that now have more pictures.
	var posts []post
	var rows []Row
	var pics [][][]byte
	for _, o := range outs {
		if o.changed {
			posts, rows, pics = append(posts, o.post), append(rows, o.row), append(pics, o.pics)
		}
	}
	if len(rows) > 0 {
		if p.Classifier != nil {
			if err := p.classify(ctx, posts, rows, pics); err != nil {
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
		q.Status, q.Attempts, q.NextAttemptAt = nextRetryState(q.Attempts, o.fixed, now, cfg)
		q.LastError, q.UpdatedAt = truncateErr(o.err), now
		updates[i] = q
		result := q.Status
		if q.Status == RetryPending {
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
	p.Log.Info("picture retries", "tried", len(due), "rewritten", len(rows), "fixed", counts[RetryFixed],
		"failed", counts["failed"], "gave_up", counts[RetryGaveUp], "pending", pending)
	return nil
}

// nextRetryState is a queue entry's state after a try.
func nextRetryState(attempts uint8, fixed bool, now time.Time, cfg RetryConfig) (string, uint8, time.Time) {
	if fixed {
		return RetryFixed, attempts, now
	}
	attempts++
	if int(attempts) >= cfg.MaxAttempts {
		return RetryGaveUp, attempts, now
	}
	return RetryPending, attempts, now.Add(cfg.Backoff[min(int(attempts), len(cfg.Backoff)-1)])
}

func truncateErr(s string) string {
	if len(s) > 300 {
		return s[:300]
	}
	return s
}
