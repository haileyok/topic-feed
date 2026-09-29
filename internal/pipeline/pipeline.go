package pipeline

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/haileyok/topic-feed/internal/chdb"
	"github.com/haileyok/topic-feed/internal/ingest"
	"github.com/haileyok/topic-feed/internal/postdoc"
)

// Config controls the pipeline loop.
type Config struct {
	Delay       time.Duration // process posts once they are at least this old (labels arrive within seconds)
	Consumer    string        // ingest_cursor row holding the pipeline's position (unix micros of indexed_at)
	BatchLimit  int           // posts per batch
	MinOCRWords int           // tesseract text is used when it has at least this many confident words
	MaxMedia    int           // attachments per post to find text for
	Poll        time.Duration // wait between batches when caught up
}

// Pipeline processes live posts into post_pipeline.
type Pipeline struct {
	Cfg        Config
	Conn       driver.Conn
	Policy     *Policy
	OCR        *OCR
	Describer  *Describer  // nil: no LLM descriptions
	Classifier *Classifier // nil: no topic predictions
	HTTP       *http.Client
	Log        *slog.Logger
}

type post struct {
	URI             string    `ch:"uri"`
	DID             string    `ch:"did"`
	IndexedAt       time.Time `ch:"indexed_at"`
	Text            string    `ch:"text"`
	MediaAlts       []string  `ch:"media_alts"`
	LinkDomain      string    `ch:"link_domain"`
	LinkTitle       string    `ch:"link_title"`
	LinkDescription string    `ch:"link_description"`
	QuoteText       string    `ch:"quote_text"`
	Tags            []string  `ch:"tags"`
	SelfLabels      []string  `ch:"self_labels"`
	MediaKinds      []string  `ch:"media_kinds"`
	MediaCIDs       []string  `ch:"media_cids"`
	MediaAltTexts   []string  `ch:"media_alt_texts"`
}

// Row mirrors the post_pipeline table.
type Row struct {
	URI              string    `ch:"uri"`
	DID              string    `ch:"did"`
	IndexedAt        time.Time `ch:"indexed_at"`
	ProcessedAt      time.Time `ch:"processed_at"`
	FeedPolicy       string    `ch:"feed_policy"`
	Labels           []string  `ch:"labels"`
	ImageTexts       []string  `ch:"image_texts"`
	ImageTextSources []string  `ch:"image_text_sources"`
	LLMCostUSD       float64   `ch:"luna_cost_usd"`
	// Topic predictions; empty for dropped posts and posts with no content.
	Model      string             `ch:"model"`
	ModelInput string             `ch:"model_input"`
	BroadProbs map[string]float32 `ch:"broad_probs"`
	PathProbs  map[string]float32 `ch:"path_probs"`
	Signals    map[string]float32 `ch:"signals"`
	Tone       map[string]float32 `ch:"tone"`
	TopBroad   string             `ch:"top_broad"`
	TopPath    string             `ch:"top_path"`
	TopPathP   float32            `ch:"top_path_p"`
}

// Run processes posts until ctx is cancelled. The first run starts at the live edge;
// later runs resume after the last processed post.
func (p *Pipeline) Run(ctx context.Context) error {
	cur := &ingest.Writer{Conn: p.Conn, Consumer: p.Cfg.Consumer}
	pos, ok, err := cur.LoadCursor(ctx)
	if err != nil {
		return fmt.Errorf("load position: %w", err)
	}
	wm := time.UnixMicro(int64(pos)).UTC()
	if !ok {
		wm = time.Now().UTC().Add(-p.Cfg.Delay)
		p.Log.Info("no saved position; starting at the live edge", "from", wm)
	}
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		upper, err := p.upperBound(ctx)
		if err != nil {
			metricErrors.WithLabelValues("position").Inc()
			p.Log.Warn("reading ingest position", "err", err)
			p.sleep(ctx, 5*time.Second)
			continue
		}
		if !upper.After(wm) {
			p.sleep(ctx, p.Cfg.Poll)
			continue
		}
		next, n, err := p.batch(ctx, wm, upper)
		if err != nil {
			if ctx.Err() != nil {
				return ctx.Err()
			}
			metricErrors.WithLabelValues("batch").Inc()
			p.Log.Warn("batch failed; retrying", "err", err, "from", wm)
			p.sleep(ctx, 5*time.Second)
			continue
		}
		if err := cur.SaveCursor(ctx, uint64(next.UnixMicro())); err != nil {
			metricErrors.WithLabelValues("position").Inc()
			p.Log.Warn("saving position", "err", err)
		}
		wm = next
		if n == 0 {
			p.sleep(ctx, p.Cfg.Poll)
		}
	}
}

func (p *Pipeline) sleep(ctx context.Context, d time.Duration) {
	select {
	case <-ctx.Done():
	case <-time.After(d):
	}
}

// upperBound is the newest ingest time the next batch may include: Delay ago, and never
// past what ingest has written. Ingest writes events in order, one flush after another,
// so every post up to the newest stored indexed_at is already in the table.
func (p *Pipeline) upperBound(ctx context.Context) (time.Time, error) {
	upper := time.Now().UTC().Add(-p.Cfg.Delay)
	var written time.Time
	if err := p.Conn.QueryRow(ctx, "SELECT max(indexed_at) FROM posts WHERE indexed_at > now() - INTERVAL 1 DAY").Scan(&written); err != nil {
		return time.Time{}, err
	}
	if written.IsZero() {
		return time.Time{}, errors.New("no posts ingested in the last day")
	}
	if written.Before(upper) {
		upper = written.UTC()
	}
	return upper, nil
}

// batch processes posts with from < indexed_at <= to and returns the position to
// resume from and how many posts it processed.
func (p *Pipeline) batch(ctx context.Context, from, to time.Time) (time.Time, int, error) {
	start := time.Now()
	var posts []post
	err := p.Conn.Select(ctx, &posts, `
		SELECT uri, did, indexed_at, text, media_alts, link_domain, link_title, link_description, quote_text, tags,
		       self_labels, media_kinds, media_cids, media_alt_texts
		FROM posts FINAL
		WHERE indexed_at > fromUnixTimestamp64Micro(toInt64(?)) AND indexed_at <= fromUnixTimestamp64Micro(toInt64(?))
		ORDER BY indexed_at, uri
		LIMIT ?`, from.UnixMicro(), to.UnixMicro(), p.Cfg.BatchLimit)
	if err != nil {
		return from, 0, fmt.Errorf("select posts: %w", err)
	}
	if len(posts) == 0 {
		return to, 0, nil
	}
	labels, err := p.labelerLabels(ctx, posts)
	if err != nil {
		return from, 0, err
	}

	rows := make([]Row, len(posts))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 64)
	for i := range posts {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer func() { <-sem; wg.Done() }()
			rows[i] = p.process(ctx, posts[i], labels)
		}(i)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return from, 0, err
	}
	if err := p.classify(ctx, posts, rows); err != nil {
		return from, 0, err
	}
	wctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), time.Minute)
	defer cancel()
	if err := chdb.Insert(wctx, p.Conn, "post_pipeline", rows); err != nil {
		return from, 0, err
	}
	for _, r := range rows {
		metricPosts.WithLabelValues(r.FeedPolicy).Inc()
	}
	last := posts[len(posts)-1].IndexedAt
	metricLag.Set(time.Since(last).Seconds())
	metricBatchSeconds.Observe(time.Since(start).Seconds())

	next := to
	if len(posts) == p.Cfg.BatchLimit {
		// More posts may share the last timestamp: resume just before it. Re-processing
		// a post replaces its row.
		next = last.Add(-time.Microsecond)
	}
	return next, len(posts), nil
}

// labelerLabels returns the current labels from the policy's labelers on each post
// and each author account, keyed by URI or DID.
func (p *Pipeline) labelerLabels(ctx context.Context, posts []post) (map[string][]string, error) {
	subjects := make([]string, 0, 2*len(posts))
	seen := map[string]bool{}
	for _, ps := range posts {
		for _, s := range []string{ps.URI, ps.DID} {
			if !seen[s] {
				seen[s] = true
				subjects = append(subjects, s)
			}
		}
	}
	var rows []struct {
		URI string `ch:"uri"`
		Val string `ch:"val"`
	}
	// The newest row per (src, uri, val) decides: a removal (neg) or an expiry clears it.
	err := p.Conn.Select(ctx, &rows, `
		SELECT uri, val FROM (
			SELECT uri, val, argMax(tuple(neg, exp), cts) AS last
			FROM mod_labels
			WHERE src IN ? AND uri IN ?
			GROUP BY src, uri, val
		)
		WHERE last.1 = 0 AND (last.2 IS NULL OR last.2 > now64(3))`, p.Policy.Labelers, subjects)
	if err != nil {
		return nil, fmt.Errorf("select labels: %w", err)
	}
	out := map[string][]string{}
	for _, r := range rows {
		out[r.URI] = append(out[r.URI], r.Val)
	}
	return out, nil
}

func (p *Pipeline) process(ctx context.Context, ps post, labeler map[string][]string) Row {
	var labels []string
	for _, group := range [][]string{ps.SelfLabels, labeler[ps.URI], labeler[ps.DID]} {
		for _, l := range group {
			if !slices.Contains(labels, l) {
				labels = append(labels, l)
			}
		}
	}
	r := Row{URI: ps.URI, DID: ps.DID, IndexedAt: ps.IndexedAt, FeedPolicy: p.Policy.Decide(labels),
		Labels: nonNil(labels), ImageTexts: []string{}, ImageTextSources: []string{},
		BroadProbs: map[string]float32{}, PathProbs: map[string]float32{}, Signals: map[string]float32{}, Tone: map[string]float32{}}
	if r.FeedPolicy == PolicyOK {
		for i := range ps.MediaCIDs {
			if len(r.ImageTexts) >= p.Cfg.MaxMedia {
				break
			}
			if i < len(ps.MediaAltTexts) && strings.TrimSpace(ps.MediaAltTexts[i]) != "" {
				continue // the author's alt text is already in the post document
			}
			text, source, cost := p.imageText(ctx, ps.MediaKinds[i], ps.DID, ps.MediaCIDs[i])
			r.ImageTexts = append(r.ImageTexts, text)
			r.ImageTextSources = append(r.ImageTextSources, source)
			r.LLMCostUSD += cost
			metricImages.WithLabelValues(source).Inc()
		}
	}
	r.ProcessedAt = time.Now().UTC()
	return r
}

// imageText finds text for one attachment: tesseract first, then an LLM description.
func (p *Pipeline) imageText(ctx context.Context, kind, did, cid string) (string, string, float64) {
	t0 := time.Now()
	fctx, cancel := context.WithTimeout(ctx, 15*time.Second)
	img, err := Fetch(fctx, p.HTTP, ThumbnailURL(kind, did, cid), 8<<20)
	cancel()
	if err != nil {
		metricErrors.WithLabelValues("fetch").Inc()
		p.Log.Debug("thumbnail fetch failed", "err", err, "did", did, "cid", cid)
		return "", SourceError, 0
	}
	octx, cancel := context.WithTimeout(ctx, 30*time.Second)
	words, err := p.OCR.Words(octx, img)
	cancel()
	metricOCRSeconds.Observe(time.Since(t0).Seconds())
	if err != nil {
		metricErrors.WithLabelValues("ocr").Inc()
		p.Log.Warn("ocr failed", "err", err)
	} else if len(words) >= p.Cfg.MinOCRWords {
		return strings.Join(words, " "), SourceOCR, 0
	}
	if p.Describer == nil {
		return "", SourceNone, 0
	}
	t1 := time.Now()
	dctx, cancel := context.WithTimeout(ctx, 45*time.Second)
	desc, cost, err := p.Describer.Describe(dctx, "data:image/jpeg;base64,"+base64.StdEncoding.EncodeToString(img))
	cancel()
	metricLLMSeconds.Observe(time.Since(t1).Seconds())
	metricLLMCost.Add(cost)
	switch {
	case errors.Is(err, ErrBudget):
		return "", SourceBudget, 0
	case err != nil:
		metricErrors.WithLabelValues("llm").Inc()
		p.Log.Warn("image description failed", "err", err)
		return "", SourceError, cost
	case desc == "":
		return "", SourceNone, cost
	}
	return desc, SourceLLM, cost
}

// ModelInput renders the post document the classifier sees: the same rendering used for
// training data, with text found in images added as alt text after the author's own.
func ModelInput(ps post, imageTexts []string) (string, bool) {
	alts := append([]string{}, ps.MediaAlts...)
	for _, t := range imageTexts {
		if strings.TrimSpace(t) != "" {
			alts = append(alts, t)
		}
	}
	doc := postdoc.New(postdoc.Input{Text: ps.Text, MediaAlts: alts, LinkDomain: ps.LinkDomain, LinkTitle: ps.LinkTitle,
		LinkDescription: ps.LinkDescription, QuoteText: ps.QuoteText, Tags: ps.Tags})
	if doc.Empty() {
		return "", false
	}
	return doc.Student(), true
}

// classify fills in predictions for every post not dropped by the label policy. A failed
// classifier call is retried; if it keeps failing the batch fails and is retried.
func (p *Pipeline) classify(ctx context.Context, posts []post, rows []Row) error {
	if p.Classifier == nil {
		return nil
	}
	var idx []int
	var texts []string
	for i := range rows {
		if rows[i].FeedPolicy == PolicyDrop {
			continue
		}
		text, ok := ModelInput(posts[i], rows[i].ImageTexts)
		if !ok {
			continue
		}
		rows[i].ModelInput = text
		idx = append(idx, i)
		texts = append(texts, text)
	}
	const chunk = 1024
	for s := 0; s < len(texts); s += chunk {
		e := min(s+chunk, len(texts))
		var (
			model string
			preds []Prediction
			err   error
		)
		t0 := time.Now()
		for attempt := 0; attempt < 6; attempt++ {
			cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			model, preds, err = p.Classifier.Classify(cctx, texts[s:e])
			cancel()
			if err == nil || ctx.Err() != nil {
				break
			}
			metricErrors.WithLabelValues("classify").Inc()
			p.Log.Warn("classifier call failed; retrying", "err", err, "attempt", attempt+1)
			p.sleep(ctx, time.Duration(1<<attempt)*time.Second)
		}
		if err != nil {
			return fmt.Errorf("classify: %w", err)
		}
		metricClassifySeconds.Observe(time.Since(t0).Seconds())
		for k, pr := range preds {
			r := &rows[idx[s+k]]
			r.Model, r.BroadProbs, r.PathProbs, r.Signals, r.Tone = model, pr.Broad, pr.Paths, pr.Signals, pr.Tone
			r.TopBroad, _ = top(pr.Broad)
			r.TopPath, r.TopPathP = top(pr.Paths)
			metricClassified.WithLabelValues(r.TopBroad).Inc()
		}
	}
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
