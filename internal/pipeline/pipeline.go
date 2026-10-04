// Package pipeline processes live posts shortly after ingest: it applies the label
// policy, downloads the pictures the topic model looks at, classifies topics, and records
// the result in post_pipeline.
package pipeline

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"slices"
	"sync"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/haileyok/topic-feed/internal/chdb"
	"github.com/haileyok/topic-feed/internal/ingest"
	"github.com/haileyok/topic-feed/internal/labelpolicy"
	"github.com/haileyok/topic-feed/internal/postdoc"
)

// Config controls the pipeline loop.
type Config struct {
	Delay       time.Duration // process posts once they are at least this old (labels arrive within seconds)
	Consumer    string        // ingest_cursor row holding the pipeline's position (unix micros of indexed_at)
	BatchLimit  int           // posts per batch
	MaxPictures int           // pictures per post the model looks at (the classifier's Health.MaxImages)
	Poll        time.Duration // wait between batches when caught up
}

// Pipeline processes live posts into post_pipeline.
type Pipeline struct {
	Cfg        Config
	Conn       driver.Conn
	Policy     *labelpolicy.Policy
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

// Row mirrors the post_pipeline columns the pipeline writes (the older image_texts,
// image_text_sources and luna_cost_usd columns are no longer written; rows from before the topic model
// looked at pictures keep them).
type Row struct {
	URI         string    `ch:"uri"`
	DID         string    `ch:"did"`
	IndexedAt   time.Time `ch:"indexed_at"`
	ProcessedAt time.Time `ch:"processed_at"`
	FeedPolicy  string    `ch:"feed_policy"`
	Labels      []string  `ch:"labels"`
	// Pictures the model should have seen (the post's first attachments, up to the model's limit) and
	// how many it did see. Fewer seen than wanted means a download failed; the retry worker tries again.
	PicturesWanted uint8 `ch:"pictures_wanted"`
	PicturesUsed   uint8 `ch:"pictures_used"`
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

// newRow is a row with the prediction maps allocated.
func newRow(uri, did string, indexedAt time.Time, policy string, labels []string) Row {
	return Row{URI: uri, DID: did, IndexedAt: indexedAt, FeedPolicy: policy, Labels: nonNil(labels),
		BroadProbs: map[string]float32{}, PathProbs: map[string]float32{}, Signals: map[string]float32{}, Tone: map[string]float32{}}
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
	pics := make([][][]byte, len(posts))
	var wg sync.WaitGroup
	sem := make(chan struct{}, 64)
	for i := range posts {
		wg.Add(1)
		sem <- struct{}{}
		go func(i int) {
			defer func() { <-sem; wg.Done() }()
			rows[i], pics[i] = p.process(ctx, posts[i], labels)
		}(i)
	}
	wg.Wait()
	if err := ctx.Err(); err != nil {
		return from, 0, err
	}
	if err := p.classify(ctx, posts, rows, pics); err != nil {
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
	return p.Policy.Current(ctx, p.Conn, subjects)
}

// process applies the label policy to a post and downloads the pictures the model looks at. Dropped
// posts are never classified, so their pictures are not downloaded.
func (p *Pipeline) process(ctx context.Context, ps post, labeler map[string][]string) (Row, [][]byte) {
	var labels []string
	for _, group := range [][]string{ps.SelfLabels, labeler[ps.URI], labeler[ps.DID]} {
		for _, l := range group {
			if !slices.Contains(labels, l) {
				labels = append(labels, l)
			}
		}
	}
	r := newRow(ps.URI, ps.DID, ps.IndexedAt, p.Policy.Decide(labels), labels)
	var pics [][]byte
	if r.FeedPolicy != labelpolicy.Drop {
		refs := pictureRefs(ps, p.Cfg.MaxPictures)
		r.PicturesWanted = uint8(len(refs))
		pics, _ = p.fetchPictures(ctx, ps.DID, refs)
		r.PicturesUsed = uint8(len(pics))
	}
	r.ProcessedAt = time.Now().UTC()
	return r, pics
}

// ModelInput renders the post document the classifier sees (post document version pd2:
// attachments and labels each get their own line). The pictures themselves go to the model
// separately, so no text read from them is added. A post with nothing but pictures still has
// content, since the model looks at the pictures.
func ModelInput(ps post, r Row, hasPictures bool) (string, bool) {
	in := postdoc.Input{Text: ps.Text, MediaAlts: ps.MediaAlts, LinkDomain: ps.LinkDomain, LinkTitle: ps.LinkTitle,
		LinkDescription: ps.LinkDescription, QuoteText: ps.QuoteText, Tags: ps.Tags, MediaKinds: ps.MediaKinds, Labels: r.Labels}
	doc := postdoc.New(in)
	if doc.Empty() && !hasPictures {
		return "", false
	}
	return doc.Student(), true
}

// classifyInFlight is how many /classify requests are sent at once. The service runs one GPU batch at a
// time, but reading the next request's JSON and pictures while the GPU works keeps the GPU busy.
const classifyInFlight = 2

// classify fills in predictions for every post not dropped by the label policy, sending each post's
// pictures (pics[i], when given) with its text. A failed classifier call is retried; if it keeps
// failing the batch fails and is retried.
func (p *Pipeline) classify(ctx context.Context, posts []post, rows []Row, pics [][][]byte) error {
	if p.Classifier == nil {
		return nil
	}
	var idx []int
	var items []Item
	for i := range rows {
		if rows[i].FeedPolicy == labelpolicy.Drop {
			continue
		}
		var pp [][]byte
		if i < len(pics) {
			pp = pics[i]
		}
		text, ok := ModelInput(posts[i], rows[i], len(pp) > 0)
		if !ok {
			continue
		}
		rows[i].ModelInput = text
		idx = append(idx, i)
		items = append(items, Item{Text: text, Pictures: pp})
	}
	cctx, cancel := context.WithCancel(ctx)
	defer cancel()
	var (
		wg       sync.WaitGroup
		mu       sync.Mutex
		firstErr error
		sem      = make(chan struct{}, classifyInFlight)
	)
	for _, c := range chunkItems(items) {
		sem <- struct{}{}
		if cctx.Err() != nil {
			<-sem
			break
		}
		wg.Add(1)
		go func(s, e int) {
			defer func() { <-sem; wg.Done() }()
			// Each chunk fills in its own rows, so chunks need no locking between them.
			if err := p.classifyChunk(cctx, items, idx, rows, s, e); err != nil {
				mu.Lock()
				if firstErr == nil {
					firstErr = err
					cancel()
				}
				mu.Unlock()
			}
		}(c[0], c[1])
	}
	wg.Wait()
	if firstErr != nil {
		return fmt.Errorf("classify: %w", firstErr)
	}
	return ctx.Err()
}

// classifyChunk classifies items[s:e] (retrying failed calls) and writes the predictions into the rows
// those items came from (rows[idx[k]] for item k).
func (p *Pipeline) classifyChunk(ctx context.Context, items []Item, idx []int, rows []Row, s, e int) error {
	var (
		model string
		preds []Prediction
		err   error
	)
	t0 := time.Now()
	for attempt := 0; attempt < 6; attempt++ {
		cctx, cancel := context.WithTimeout(ctx, 90*time.Second)
		model, preds, err = p.Classifier.Classify(cctx, items[s:e])
		cancel()
		if err == nil || ctx.Err() != nil {
			break
		}
		metricErrors.WithLabelValues("classify").Inc()
		p.Log.Warn("classifier call failed; retrying", "err", err, "attempt", attempt+1)
		p.sleep(ctx, time.Duration(1<<attempt)*time.Second)
	}
	if err != nil {
		return err
	}
	metricClassifySeconds.Observe(time.Since(t0).Seconds())
	for k, pr := range preds {
		r := &rows[idx[s+k]]
		r.Model, r.BroadProbs, r.PathProbs, r.Signals, r.Tone = model, pr.Broad, pr.Paths, pr.Signals, pr.Tone
		r.TopBroad, _ = top(pr.Broad)
		r.TopPath, r.TopPathP = top(pr.Paths)
		r.PicturesUsed = 0
		if sent := len(items[s+k].Pictures); sent > 0 {
			r.PicturesUsed = uint8(min(pr.PicturesUsed, sent)) // the service may not read every file
		}
		if r.PicturesUsed == 0 {
			// The meme score only means something for a post whose picture the model saw (it was
			// never trained on text-only posts), so it isn't stored for the others. Feeds treat a
			// missing score as 0.
			delete(r.Signals, "meme")
		}
		metricClassified.WithLabelValues(r.TopBroad).Inc()
	}
	return nil
}

func nonNil(s []string) []string {
	if s == nil {
		return []string{}
	}
	return s
}
