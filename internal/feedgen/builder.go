package feedgen

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/labstack/echo/v4"
	"golang.org/x/time/rate"

	"github.com/haileyok/topic-feed/internal/taxonomy"
)

// Previewer answers the feed builder page: it builds and ranks a feed from settings the
// page sends, with the same code as the published feeds. It is public, so it never shows
// adult content, limits how often each visitor can run a new build and how many run at
// once, and serves repeated settings from a short cache.
type Previewer struct {
	Builder  Builder
	Tax      *taxonomy.Taxonomy
	Log      *slog.Logger
	Window   time.Duration // how far back previews go
	MaxPosts int           // candidates per preview
	CacheFor time.Duration

	paths    map[string]bool
	volumes  map[string]float64 // posts per hour per topic, refreshed by Run
	slots    chan struct{}      // concurrent builds
	mu       sync.Mutex
	cache    map[string]*previewBuild
	visitors map[string]*visitor
}

type previewBuild struct {
	posts   []Post
	removed Removed
	builtAt time.Time
	took    time.Duration
}

type visitor struct {
	lim  *rate.Limiter
	seen time.Time
}

// NewPreviewer makes a previewer; call Run to prune its caches.
func NewPreviewer(b Builder, tax *taxonomy.Taxonomy, log *slog.Logger) *Previewer {
	return &Previewer{Builder: b, Tax: tax, Log: log, Window: 48 * time.Hour, MaxPosts: 5000, CacheFor: 30 * time.Second,
		paths: TaxonomyPaths(tax), slots: make(chan struct{}, 3),
		cache: map[string]*previewBuild{}, visitors: map[string]*visitor{}}
}

// volumer reports recent posts per hour per topic. *Store implements it.
type volumer interface {
	Volumes(ctx context.Context) (map[string]float64, error)
}

func (p *Previewer) refreshVolumes(ctx context.Context) {
	v, ok := p.Builder.(volumer)
	if !ok {
		return
	}
	vctx, cancel := context.WithTimeout(ctx, 30*time.Second)
	defer cancel()
	vols, err := v.Volumes(vctx)
	if err != nil {
		p.Log.Error("topic volumes", "err", err)
		return
	}
	p.mu.Lock()
	p.volumes = vols
	p.mu.Unlock()
}

// Run refreshes topic volumes every 10 minutes and prunes expired cache entries and idle
// visitors every minute, until ctx ends.
func (p *Previewer) Run(ctx context.Context) {
	p.refreshVolumes(ctx)
	t := time.NewTicker(time.Minute)
	defer t.Stop()
	for i := 1; ; i++ {
		select {
		case <-ctx.Done():
			return
		case now := <-t.C:
			if i%10 == 0 {
				p.refreshVolumes(ctx)
			}
			p.mu.Lock()
			for k, b := range p.cache {
				if now.Sub(b.builtAt) > p.CacheFor {
					delete(p.cache, k)
				}
			}
			for ip, v := range p.visitors {
				if now.Sub(v.seen) > 10*time.Minute {
					delete(p.visitors, ip)
				}
			}
			p.mu.Unlock()
		}
	}
}

// PreviewSpec is the feed the builder page describes; it mirrors a feeds.yaml entry.
type PreviewSpec struct {
	Paths   []string           `json:"paths"`
	MinProb float32            `json:"min_prob"`
	Exclude map[string]float32 `json:"exclude,omitempty"`
	Tone    Rules              `json:"tone"`
	Signals Rules              `json:"signals"`
	Ranking *Ranking           `json:"ranking,omitempty"`
	Offset  int                `json:"offset,omitempty"`
	Limit   int                `json:"limit,omitempty"`
}

// adultCutoff keeps posts the model reads as adult out of previews, even unlabeled ones.
const adultCutoff = 0.2

var errNoAdult = errors.New("adult topics aren't available in the builder")

// feed turns a spec into a validated Feed.
func (p *Previewer) feed(s PreviewSpec) (Feed, error) {
	f := Feed{Rkey: "preview", DisplayName: "Preview", Paths: s.Paths, MinProb: s.MinProb,
		Exclude: map[string]float32{}, Tone: s.Tone, Signals: s.Signals, Ranking: DefaultRanking}
	if s.Ranking != nil {
		f.Ranking = *s.Ranking
	}
	if len(f.Paths) == 0 {
		return f, errors.New("pick at least one topic")
	}
	if len(f.Paths) > 150 || len(s.Exclude) > 150 {
		return f, errors.New("too many topics")
	}
	for _, path := range f.Paths {
		if path == "adult_content" || strings.HasPrefix(path, "adult_content/") {
			return f, errNoAdult
		}
	}
	for k, v := range s.Exclude {
		f.Exclude[k] = v
	}
	if v, ok := f.Exclude["adult_content"]; !ok || v > adultCutoff {
		f.Exclude["adult_content"] = adultCutoff
	}
	if r := f.Ranking; r.Gravity > 4 || r.FreshEvery > 20 || r.AuthorGap > 50 || r.PromoPenalty > 10 ||
		r.Weights.Like > 20 || r.Weights.Repost > 20 || r.Weights.Reply > 20 || r.Weights.Quote > 20 {
		return f, errors.New("ranking settings out of range")
	}
	if err := (&Config{Feeds: []Feed{f}}).Validate(p.paths); err != nil {
		// Validate prefixes the feed name; the page only needs the reason.
		return f, errors.New(strings.TrimPrefix(err.Error(), `feed "preview": `))
	}
	return f, nil
}

// key identifies a spec's build, ignoring the page it asks for.
func key(f Feed) string {
	b, _ := json.Marshal(struct {
		Paths   []string
		MinProb float32
		Exclude map[string]float32
		Tone    Rules
		Signals Rules
		Ranking Ranking
	}{f.Paths, f.MinProb, f.Exclude, f.Tone, f.Signals, f.Ranking})
	sum := sha256.Sum256(b)
	return hex.EncodeToString(sum[:])
}

type previewPost struct {
	URI        string             `json:"uri"`
	Score      float64            `json:"score"`
	Match      float32            `json:"match"`
	AgeMinutes int                `json:"age_minutes"`
	Likes      uint64             `json:"likes"`
	Reposts    uint64             `json:"reposts"`
	Replies    uint64             `json:"replies"`
	Quotes     uint64             `json:"quotes"`
	Topic      string             `json:"topic"`
	P          float32            `json:"p"`
	Top        [][2]any           `json:"top"`
	Tone       map[string]float32 `json:"tone"`
	Signals    map[string]float32 `json:"signals"`
}

type previewResponse struct {
	Posts   []previewPost  `json:"posts"`
	Total   int            `json:"total"`
	Removed map[string]int `json:"removed"`
	TookMS  int64          `json:"took_ms"`
	BuiltAt time.Time      `json:"built_at"`
	Cached  bool           `json:"cached"`
}

// Handle answers POST /api/preview.
func (p *Previewer) Handle(c echo.Context) error {
	fail := func(status int, msg string) error {
		metricPreviews.WithLabelValues(http.StatusText(status)).Inc()
		return c.JSON(status, map[string]string{"error": msg})
	}
	var spec PreviewSpec
	if err := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, 64<<10)).Decode(&spec); err != nil {
		return fail(http.StatusBadRequest, "couldn't read the feed settings")
	}
	f, err := p.feed(spec)
	if err != nil {
		return fail(http.StatusBadRequest, err.Error())
	}
	limit := spec.Limit
	if limit <= 0 || limit > 100 {
		limit = 30
	}
	k := key(f)

	p.mu.Lock()
	b, cached := p.cache[k]
	if cached && time.Since(b.builtAt) > p.CacheFor {
		cached = false
	}
	p.mu.Unlock()

	if !cached {
		if !p.visitor(clientIP(c)).Allow() {
			return fail(http.StatusTooManyRequests, "slow down a little: too many changes at once")
		}
		ctx, cancel := context.WithTimeout(c.Request().Context(), 20*time.Second)
		defer cancel()
		select {
		case p.slots <- struct{}{}:
		case <-ctx.Done():
			return fail(http.StatusServiceUnavailable, "the builder is busy; try again in a moment")
		}
		start := time.Now()
		posts, rm, err := p.Builder.Build(ctx, f, start.Add(-p.Window), p.MaxPosts)
		<-p.slots
		if err != nil {
			p.Log.Error("preview build", "err", err)
			return fail(http.StatusInternalServerError, "couldn't build the feed")
		}
		b = &previewBuild{posts: Rank(posts, f, start), removed: rm, builtAt: start, took: time.Since(start)}
		p.mu.Lock()
		p.cache[k] = b
		p.mu.Unlock()
		metricPreviewSeconds.Observe(b.took.Seconds())
	}

	resp := previewResponse{Posts: []previewPost{}, Total: len(b.posts), TookMS: b.took.Milliseconds(),
		BuiltAt: b.builtAt, Cached: cached,
		Removed: map[string]int{"deleted": b.removed.Deleted, "inactive": b.removed.Inactive, "labeled": b.removed.Labeled}}
	start := min(max(spec.Offset, 0), len(b.posts))
	end := min(start+limit, len(b.posts))
	for _, post := range b.posts[start:end] {
		top := make([][2]any, 0, len(post.TopPaths))
		for i, path := range post.TopPaths {
			if i < len(post.TopPs) {
				top = append(top, [2]any{path, round3(post.TopPs[i])})
			}
		}
		resp.Posts = append(resp.Posts, previewPost{URI: post.URI, Score: post.Score, Match: post.Match,
			AgeMinutes: int(time.Since(post.IndexedAt).Minutes()), Likes: post.Likes, Reposts: post.Reposts,
			Replies: post.Replies, Quotes: post.Quotes, Topic: post.TopPath, P: post.TopPathP, Top: top,
			Tone: post.Tone, Signals: post.Signals})
	}
	metricPreviews.WithLabelValues("OK").Inc()
	return c.JSON(http.StatusOK, resp)
}

func (p *Previewer) visitor(ip string) *rate.Limiter {
	p.mu.Lock()
	defer p.mu.Unlock()
	v, ok := p.visitors[ip]
	if !ok {
		// A burst for dragging a slider around, then one new build a second.
		v = &visitor{lim: rate.NewLimiter(rate.Every(time.Second), 10)}
		p.visitors[ip] = v
	}
	v.seen = time.Now()
	return v.lim
}

// clientIP is the visitor's address: Cloudflare's header when the request came through
// the tunnel (the server only listens on localhost), else the connection's.
func clientIP(c echo.Context) string {
	if ip := c.Request().Header.Get("CF-Connecting-IP"); ip != "" {
		return ip
	}
	host, _, err := net.SplitHostPort(c.Request().RemoteAddr)
	if err != nil {
		return c.Request().RemoteAddr
	}
	return host
}

type taxonomyTopic struct {
	ID          string          `json:"id"`
	Name        string          `json:"name"`
	Description string          `json:"description"`
	Subtopics   []taxonomyTopic `json:"subtopics,omitempty"`
}

// HandleTaxonomy answers GET /api/taxonomy: the topics the builder offers (adult topics
// and "unclear" are left out), and the tone and signal names.
func (p *Previewer) HandleTaxonomy(c echo.Context) error {
	var broad []taxonomyTopic
	for _, b := range p.Tax.Broad {
		if b.ID == "adult_content" || b.ID == "unclear" {
			continue
		}
		t := taxonomyTopic{ID: b.ID, Name: b.Name, Description: b.Description}
		for _, s := range b.Subtopics {
			t.Subtopics = append(t.Subtopics, taxonomyTopic{ID: b.ID + "/" + s.ID, Name: s.Name, Description: s.Description})
		}
		broad = append(broad, t)
	}
	p.mu.Lock()
	vols := map[string]int{}
	for k, v := range p.volumes {
		vols[k] = int(v + 0.5)
	}
	p.mu.Unlock()
	c.Response().Header().Set("Cache-Control", "public, max-age=300")
	return c.JSON(http.StatusOK, map[string]any{"version": p.Tax.Version, "topics": broad,
		"tones": Tones, "signals": Signals, "default_ranking": DefaultRanking, "per_hour": vols})
}
