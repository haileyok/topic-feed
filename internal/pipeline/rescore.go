package pipeline

import (
	"context"
	"fmt"
	"sync"
	"time"

	"github.com/haileyok/topic-feed/internal/chdb"
)

// rescoreRow is one post_pipeline row joined with its posts row.
type rescoreRow struct {
	post
	ProcessedAt    time.Time `ch:"processed_at"`
	FeedPolicy     string    `ch:"feed_policy"`
	Labels         []string  `ch:"labels"`
	PicturesWanted uint8     `ch:"pictures_wanted"`
	PicturesUsed   uint8     `ch:"pictures_used"`
}

// rescoreSelect is the start of the query that reads rescoreRow; callers add the FROM clause.
const rescoreSelect = `
		SELECT p.uri AS uri, p.did AS did, p.indexed_at AS indexed_at, p.text AS text, p.media_alts AS media_alts,
		       p.link_domain AS link_domain, p.link_title AS link_title, p.link_description AS link_description,
		       p.quote_text AS quote_text, p.tags AS tags, p.self_labels AS self_labels, p.media_kinds AS media_kinds,
		       p.media_cids AS media_cids, p.media_alt_texts AS media_alt_texts,
		       pp.processed_at AS processed_at, pp.feed_policy AS feed_policy, pp.labels AS labels,
		       pp.pictures_wanted AS pictures_wanted, pp.pictures_used AS pictures_used`

// row is the post_pipeline row to write when re-classifying this post: it keeps the original
// processed_at plus 1ms, so the new row replaces the old one (post_pipeline is a ReplacingMergeTree
// on processed_at) without changing how late the post was processed.
func (r rescoreRow) row() Row {
	out := newRow(r.URI, r.DID, r.IndexedAt, r.FeedPolicy, r.Labels)
	out.ProcessedAt = r.ProcessedAt.Add(time.Millisecond)
	out.PicturesWanted, out.PicturesUsed = r.PicturesWanted, r.PicturesUsed
	return out
}

// rescoreWindow is one time window of posts, read and with its pictures downloaded, ready to classify.
type rescoreWindow struct {
	start time.Time
	posts []post
	rows  []Row
	pics  [][][]byte

	// Where the time went preparing the window (logged with the window's result).
	selectDur, fetchDur time.Duration
	pictures, missing   int       // pictures wanted, and how many could not be downloaded
	readyAt             time.Time // when the window was ready to classify
}

// prepareWindow reads the posts the old model classified in [lo, end) and downloads their pictures.
func (p *Pipeline) prepareWindow(ctx context.Context, start, lo, end time.Time, oldModel string) (rescoreWindow, error) {
	w := rescoreWindow{start: start}
	var rs []rescoreRow
	t0 := time.Now()
	if err := p.Conn.Select(ctx, &rs, rescoreSelect+`
		FROM (SELECT * FROM post_pipeline FINAL WHERE indexed_at >= ? AND indexed_at < ? AND model = ?) AS pp
		INNER JOIN (SELECT * FROM posts FINAL WHERE indexed_at >= ? AND indexed_at < ?) AS p ON p.uri = pp.uri`,
		lo, end, oldModel, lo, end); err != nil {
		return w, fmt.Errorf("select %s: %w", start, err)
	}
	w.selectDur = time.Since(t0)
	w.posts = make([]post, len(rs))
	w.rows = make([]Row, len(rs))
	w.pics = make([][][]byte, len(rs))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 64)
	t0 = time.Now()
	for i, r := range rs {
		w.posts[i], w.rows[i] = r.post, r.row()
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer func() { <-sem; wg.Done() }()
			refs := pictureRefs(w.posts[i], p.Cfg.MaxPictures)
			w.pics[i], _ = p.fetchPictures(ctx, w.posts[i].DID, refs)
			w.rows[i].PicturesWanted, w.rows[i].PicturesUsed = uint8(len(refs)), uint8(len(w.pics[i]))
		}(i)
	}
	wg.Wait()
	w.fetchDur = time.Since(t0)
	for _, r := range w.rows {
		w.pictures += int(r.PicturesWanted)
		w.missing += int(r.PicturesWanted) - int(r.PicturesUsed)
	}
	w.readyAt = time.Now()
	return w, ctx.Err()
}

// Rescore re-classifies posts that model oldModel classified between `after` and `before` (by
// indexed_at; a zero `after` means from the first one), with the current classifier, reusing the
// labels and label policy decision the pipeline already stored but downloading each post's pictures
// again (the model looks at them). A post whose pictures are gone is classified without them. Only
// rows still on oldModel are picked, so a rerun resumes.
//
// The next window is read and its pictures downloaded while the current one is being classified, so
// the GPU isn't left waiting for the network.
func (p *Pipeline) Rescore(ctx context.Context, oldModel string, after, before time.Time, window time.Duration) error {
	if after.IsZero() {
		after = time.Unix(0, 0).UTC()
	}
	var first time.Time
	var n uint64
	if err := p.Conn.QueryRow(ctx, `SELECT min(indexed_at), count() FROM post_pipeline FINAL WHERE model = ? AND indexed_at >= ? AND indexed_at < ?`,
		oldModel, after, before).Scan(&first, &n); err != nil {
		return err
	}
	if n == 0 {
		p.Log.Info("nothing to rescore", "model", oldModel)
		return nil
	}
	p.Log.Info("rescoring", "from_model", oldModel, "posts", n, "from", first, "before", before, "max_pictures", p.Cfg.MaxPictures)

	pctx, cancel := context.WithCancel(ctx)
	defer cancel()
	windows := make(chan rescoreWindow, 1) // one ready window waits while the next is being prepared
	prepErr := make(chan error, 1)
	go func() {
		defer close(windows)
		for t := first.Truncate(window); t.Before(before); t = t.Add(window) {
			end := t.Add(window)
			if end.After(before) {
				end = before
			}
			lo := t // the first window may start before `after`
			if after.After(lo) {
				lo = after
			}
			w, err := p.prepareWindow(pctx, t, lo, end, oldModel)
			if err != nil {
				prepErr <- err
				return
			}
			if len(w.posts) == 0 {
				continue
			}
			select {
			case windows <- w:
			case <-pctx.Done():
				return
			}
		}
	}()

	done, start := 0, time.Now()
	for {
		tWait := time.Now()
		w, ok := <-windows
		if !ok {
			break
		}
		recvWait := time.Since(tWait)   // the classifier sat idle this long waiting for the next window to be prepared
		queued := time.Since(w.readyAt) // how long the finished window waited for the classifier
		tClassify := time.Now()
		if err := p.classify(ctx, w.posts, w.rows, w.pics); err != nil {
			return err
		}
		classifyDur := time.Since(tClassify)
		// Keep only rows the new model classified; anything it skipped keeps its old row.
		out := w.rows[:0]
		for _, r := range w.rows {
			if r.Model != "" && r.Model != oldModel {
				out = append(out, r)
			}
		}
		tInsert := time.Now()
		wctx, wcancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
		err := chdb.Insert(wctx, p.Conn, "post_pipeline", out)
		wcancel()
		if err != nil {
			return fmt.Errorf("insert %s: %w", w.start, err)
		}
		insertDur := time.Since(tInsert)
		done += len(out)
		rate := float64(done) / time.Since(start).Seconds()
		p.Log.Info("rescored", "window", w.start.Format(time.RFC3339), "posts", len(out), "done", done, "of", n,
			"per_second", int(rate), "eta", (time.Duration(float64(int(n)-done)/max(rate, 1e-9)) * time.Second).Round(time.Minute).String(),
			"pictures", w.pictures, "pictures_missing", w.missing,
			"select_ms", w.selectDur.Milliseconds(), "fetch_ms", w.fetchDur.Milliseconds(),
			"recv_wait_ms", recvWait.Milliseconds(), "ready_queued_ms", queued.Milliseconds(),
			"classify_ms", classifyDur.Milliseconds(), "insert_ms", insertDur.Milliseconds())
	}
	select {
	case err := <-prepErr:
		return err
	default:
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	p.Log.Info("rescore done", "posts", done)
	return nil
}
