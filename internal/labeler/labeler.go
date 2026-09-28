// Package labeler labels posts with hosted Jev (plan §10): pass 1 asks the broad
// topic and ranking signals, pass 2 asks subtopics for each post's top broad topics.
package labeler

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"math"
	"sort"
	"strings"
	"sync"
	"time"

	typesafe "github.com/haileyok/typesafe-client/go"
	"golang.org/x/time/rate"

	"github.com/haileyok/topic-feed/internal/postdoc"
	"github.com/haileyok/topic-feed/internal/taxonomy"
)

// Config controls a labeling run.
type Config struct {
	Model       string  // pinned Jev model, e.g. jev-1.13.0
	BatchSize   int     // posts per request (1 until the Phase 0 test picks a size)
	TopK        int     // broad topics per post that get a subtopic question
	MinBroadP   float64 // skip broad candidates below this probability
	Source      string  // window|sample|uncertain|eval
	Concurrency int     // requests in flight
	MaxRPM      int     // request budget per minute
	MaxTPS      int     // estimated input-token budget per second
}

// LabelConfig is the short hash identifying everything that shapes a label. Two runs
// with the same label_config produce comparable labels.
func LabelConfig(t *taxonomy.Taxonomy, c Config) string {
	s := strings.Join([]string{
		t.Version, t.Hash, QuestionsVersion, postdoc.Version, c.Model,
		fmt.Sprintf("b%d", c.BatchSize), fmt.Sprintf("k%d", c.TopK), fmt.Sprintf("m%g", c.MinBroadP),
	}, "|")
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])[:12]
}

// Post is one post to label.
type Post struct {
	URI string
	Doc postdoc.Doc
}

// Label is one post's result, matching the jev_labels table.
type Label struct {
	URI             string
	JevModel        string
	BroadProbs      map[string]float32
	BroadConfidence float32
	SubProbs        map[string]float32 // "broad/sub": P(sub | broad)
	PathScores      map[string]float32 // "broad/sub": sqrt(P(broad) * P(sub | broad))
	Signals         map[string]float32
	RequestIDs      []string
	LabeledAt       time.Time
}

// RequestLog is one Jev request, matching the jev_requests table.
type RequestLog struct {
	RequestID   string
	TS          time.Time
	Pass        string
	NPosts      int
	NQuestions  int
	InputTokens int64
	LatencyMS   int64
	Status      string
	Error       string
}

// Labeler runs the two passes against Jev under a rate budget.
type Labeler struct {
	Cfg    Config
	Tax    *taxonomy.Taxonomy
	Client *typesafe.Client
	Log    *slog.Logger

	rpm *rate.Limiter
	tps *rate.Limiter

	mu       sync.Mutex
	requests []RequestLog
}

func New(cfg Config, tax *taxonomy.Taxonomy, client *typesafe.Client, log *slog.Logger) *Labeler {
	return &Labeler{
		Cfg:    cfg,
		Tax:    tax,
		Client: client,
		Log:    log,
		rpm:    rate.NewLimiter(rate.Limit(float64(cfg.MaxRPM)/60), max(1, cfg.MaxRPM/60)),
		tps:    rate.NewLimiter(rate.Limit(cfg.MaxTPS), cfg.MaxTPS),
	}
}

// DrainRequests returns and clears the request log collected so far.
func (l *Labeler) DrainRequests() []RequestLog {
	l.mu.Lock()
	defer l.mu.Unlock()
	out := l.requests
	l.requests = nil
	return out
}

// LabelBatch labels up to BatchSize posts: pass 1, then pass 2 for the subtopics.
func (l *Labeler) LabelBatch(ctx context.Context, posts []Post) ([]Label, error) {
	state := make([]postdoc.JevPost, len(posts))
	for i, p := range posts {
		state[i] = p.Doc.Jev()
	}

	// Pass 1: broad topic + ranking signals.
	q1 := typesafe.Questions{}
	for i := range posts {
		pass1Questions(l.Tax, i, q1)
	}
	r1, err := l.call(ctx, "broad", state, q1, len(posts))
	if err != nil {
		return nil, err
	}

	labels := make([]Label, len(posts))
	type pair struct {
		post  int
		broad taxonomy.Topic
	}
	var pairs []pair
	for i, p := range posts {
		lb := Label{
			URI:        p.URI,
			JevModel:   r1.Model,
			SubProbs:   map[string]float32{},
			PathScores: map[string]float32{},
			Signals:    map[string]float32{},
			RequestIDs: []string{r1.RequestID},
		}
		broad, ok := r1.Choice(fmt.Sprintf("p%d_broad", i))
		if !ok {
			return nil, fmt.Errorf("missing broad answer for post %d", i)
		}
		lb.BroadProbs = toF32(broad.Probabilities)
		lb.BroadConfidence = float32(broad.Confidence)
		l.readSignals(r1, i, lb.Signals)
		labels[i] = lb

		for rank, op := range broad.Ranked() {
			if rank >= l.Cfg.TopK || op.Probability < l.Cfg.MinBroadP {
				break
			}
			if b, ok := l.Tax.BroadByID(op.Option); ok && len(b.Subtopics) > 0 {
				pairs = append(pairs, pair{i, b})
			}
		}
	}

	// Pass 2: subtopics for each (post, broad candidate). With batch size 1 this is a
	// single request; larger batches would group by broad topic (plan §10.4).
	if len(pairs) > 0 {
		q2 := typesafe.Questions{}
		for _, pr := range pairs {
			q2[subQuestionID(pr.post, pr.broad.ID)] = pass2Question(pr.broad, pr.post)
		}
		r2, err := l.call(ctx, "sub", state, q2, len(posts))
		if err != nil {
			return nil, err
		}
		for _, pr := range pairs {
			ans, ok := r2.Choice(subQuestionID(pr.post, pr.broad.ID))
			if !ok {
				return nil, fmt.Errorf("missing subtopic answer %s", subQuestionID(pr.post, pr.broad.ID))
			}
			lb := &labels[pr.post]
			pb := float64(lb.BroadProbs[pr.broad.ID])
			for sub, ps := range ans.Probabilities {
				key := pr.broad.ID + "/" + sub
				lb.SubProbs[key] = float32(ps)
				lb.PathScores[key] = float32(math.Sqrt(pb * ps))
			}
		}
		for _, pr := range pairs {
			lb := &labels[pr.post]
			if len(lb.RequestIDs) == 1 {
				lb.RequestIDs = append(lb.RequestIDs, r2.RequestID)
			}
		}
	}

	now := time.Now().UTC()
	for i := range labels {
		labels[i].LabeledAt = now
	}
	return labels, nil
}

func (l *Labeler) readSignals(r *typesafe.Response, i int, out map[string]float32) {
	id := func(s string) string { return fmt.Sprintf("p%d_%s", i, s) }
	if s, ok := r.Score(id(sigSubstance)); ok {
		// Levels are numbered from 0, so this maps the score onto 0..1.
		out[sigSubstance] = float32(s.Score / float64(len(substanceLevels)-1))
	}
	for _, n := range []string{sigNews, sigPromo, sigGeneralInterest} {
		if a, ok := r.Noul(id(n)); ok {
			out[n] = float32(a.Noul)
		}
	}
	if t, ok := r.Choice(id(sigTone)); ok {
		for opt, p := range t.Probabilities {
			out["tone."+opt] = float32(p)
		}
	}
}

// call sends one request under the rate budget and records it in the request log.
func (l *Labeler) call(ctx context.Context, pass string, state []postdoc.JevPost, q typesafe.Questions, nPosts int) (*typesafe.Response, error) {
	est := estimateTokens(state, q)
	if err := l.rpm.Wait(ctx); err != nil {
		return nil, err
	}
	if err := l.tps.WaitN(ctx, min(est, l.Cfg.MaxTPS)); err != nil {
		return nil, err
	}
	start := time.Now()
	resp, err := l.Client.SystemOne(ctx, typesafe.Request{
		State:     map[string]any{"posts": state},
		Questions: q,
		Model:     l.Cfg.Model,
	})
	rl := RequestLog{TS: start.UTC(), Pass: pass, NPosts: nPosts, NQuestions: len(q), LatencyMS: time.Since(start).Milliseconds(), Status: "ok"}
	if err != nil {
		rl.Status = "error"
		rl.Error = err.Error()
		var apiErr *typesafe.APIError
		if errors.As(err, &apiErr) {
			rl.RequestID = apiErr.RequestID
			if apiErr.StatusCode == 429 {
				rl.Status = "rate_limited"
			}
		}
	} else {
		rl.RequestID = resp.RequestID
		rl.InputTokens = resp.Usage.InputTokens
	}
	l.mu.Lock()
	l.requests = append(l.requests, rl)
	l.mu.Unlock()
	return resp, err
}

// estimateTokens approximates input tokens as characters/4 of the JSON request.
func estimateTokens(state []postdoc.JevPost, q typesafe.Questions) int {
	b1, _ := json.Marshal(state)
	b2, _ := json.Marshal(q)
	return (len(b1) + len(b2)) / 4
}

func toF32(m map[string]float64) map[string]float32 {
	out := make(map[string]float32, len(m))
	for k, v := range m {
		out[k] = float32(v)
	}
	return out
}

// BestPath returns the highest path score key ("broad/sub"), or the top broad topic
// alone when no subtopic was asked (topics without subtopics).
func (lb Label) BestPath() string {
	type kv struct {
		k string
		v float32
	}
	var paths []kv
	for k, v := range lb.PathScores {
		paths = append(paths, kv{k, v})
	}
	if len(paths) == 0 {
		best, bp := "", float32(-1)
		for k, v := range lb.BroadProbs {
			if v > bp {
				best, bp = k, v
			}
		}
		return best
	}
	sort.Slice(paths, func(i, j int) bool {
		return paths[i].v > paths[j].v || (paths[i].v == paths[j].v && paths[i].k < paths[j].k)
	})
	return paths[0].k
}
