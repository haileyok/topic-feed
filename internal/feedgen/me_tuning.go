package feedgen

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/haileyok/topic-feed/internal/signin"
)

const (
	// maxTuningBody bounds a tuning sent to be saved or previewed: at most 60 topics fit in
	// a few kilobytes.
	maxTuningBody = 16 << 10
	// previewPosts is how many posts a preview shows.
	previewPosts = 30
	// saveTimeout bounds saving a tuning, which may read the viewer's likes again.
	saveTimeout    = 25 * time.Second
	previewTimeout = 25 * time.Second
	readTimeout    = 10 * time.Second
	textTimeout    = 3 * time.Second
)

// PostTextSource reads what posts say. *Store implements it.
type PostTextSource interface {
	// PostTexts returns the text of the posts it has, by URI.
	PostTexts(ctx context.Context, uris []string) (map[string]string, error)
}

// TopicChoice is a subtopic a viewer can add to their feed.
type TopicChoice struct {
	Path  string `json:"path"`
	Name  string `json:"name"`
	Broad string `json:"broad"`
}

// RankingValues are the numbers a feed ranks with (see Ranking).
type RankingValues struct {
	Gravity      float64 `json:"gravity"`
	FreshEvery   int     `json:"freshEvery"`
	PromoPenalty float64 `json:"promoPenalty"`
	Like         float64 `json:"like"`
	Repost       float64 `json:"repost"`
	Reply        float64 `json:"reply"`
	Quote        float64 `json:"quote"`
}

// TuningDefaults is what each setting is until the viewer changes it.
type TuningDefaults struct {
	Freshness    string  `json:"freshness"`
	AuthorGap    int     `json:"authorGap"`
	HalfLifeDays float64 `json:"halfLifeDays"`
	LookbackDays int     `json:"lookbackDays"`
	MinLikes     int     `json:"minLikes"`
	Interests    int     `json:"interests"`
	WindowHours  int     `json:"windowHours"`
	MinTopicProb float64 `json:"minTopicProb"`
	MaxServes    int     `json:"maxServes"`
	ListSize     int     `json:"listSize"`
	// MinEngagement is how much reaction a post needs before the feed shows it (0: none).
	MinEngagement float64 `json:"minEngagement"`
	// Ranking is the feed's ranking for each freshness setting (popular, balanced, fresh): the
	// ranking settings start from the one the viewer has chosen.
	Ranking map[string]RankingValues `json:"ranking"`
}

// TuningLimits are the furthest each setting can be moved.
type TuningLimits struct {
	Weight       float64 `json:"weight"` // of an interest
	Boost        float64 `json:"boost"`  // of a tone or signal, either way
	Gravity      float64 `json:"gravity"`
	FreshEvery   int     `json:"freshEvery"`
	PromoPenalty float64 `json:"promoPenalty"`
	Engagement   float64 `json:"engagement"` // of a like, repost, reply or quote
	AuthorGap    int     `json:"authorGap"`
	LookbackDays int     `json:"lookbackDays"`
	MinLikes     int     `json:"minLikes"`
	Interests    int     `json:"interests"`
	WindowHours  int     `json:"windowHours"`  // the feed's own, which can't be reached past
	MinTopicProb float64 `json:"minTopicProb"` // the feed's own, which can't be gone below
	MaxServes    int     `json:"maxServes"`
	ListSize     int     `json:"listSize"` // the feed's own, which can't be exceeded
	// MinEngagement is the most a viewer can ask of a post.
	MinEngagement float64 `json:"minEngagement"`
}

// TuningResponse is a viewer's saved tuning with what the page needs to offer controls for it.
type TuningResponse struct {
	Tuning    Tuning         `json:"tuning"`
	Defaults  TuningDefaults `json:"defaults"`
	Limits    TuningLimits   `json:"limits"`
	MaxWeight float64        `json:"maxWeight"`
	// Tones and Signals are the names of the model's scores that cutoffs and boosts can use.
	Tones   []string `json:"tones"`
	Signals []string `json:"signals"`
	// Topics are the subtopics that can be added, by broad topic and name.
	Topics []TopicChoice `json:"topics"`
}

// TopicProb is how sure the model is that a post is about a topic.
type TopicProb struct {
	Path string  `json:"path"`
	Name string  `json:"name"`
	P    float64 `json:"p"`
}

// PreviewPost is a post in a preview, with what the feed knows about it.
type PreviewPost struct {
	URI  string `json:"uri"`
	URL  string `json:"url"`
	DID  string `json:"did"`
	Text string `json:"text"` // "" if it couldn't be read
	// Topic is the subtopic the post was picked for (the model's most likely one), and Top its
	// three most likely, each with how sure the model is.
	Topic     string      `json:"topic"`
	TopicPath string      `json:"topicPath"`
	Broad     string      `json:"broad"`
	Top       []TopicProb `json:"top"`
	// Tone sums to 1 over the six tones; Signals are independent scores, each from 0 to 1.
	Tone    map[string]float64 `json:"tone"`
	Signals map[string]float64 `json:"signals"`
	Labels  []string           `json:"labels"`
	// Score is what the post ranked by (engagement and a quality prior, over age).
	Score     float64   `json:"score"`
	IndexedAt time.Time `json:"indexedAt"`
	// The engagement the ranking counted.
	Likes   uint64 `json:"likes"`
	Reposts uint64 `json:"reposts"`
	Replies uint64 `json:"replies"`
	Quotes  uint64 `json:"quotes"`
}

// PreviewInterest is a topic a draft builds the feed from, and its share of the feed.
type PreviewInterest struct {
	Path  string  `json:"path"`
	Name  string  `json:"name"`
	Broad string  `json:"broad"`
	Share float64 `json:"share"`
}

// PreviewResponse is the first posts a draft tuning would put in the viewer's feed.
type PreviewResponse struct {
	// State is StatePersonal, or StateGeneric when the draft leaves the feed a mix of every topic.
	State string `json:"state"`
	// Interests are the topics the feed is built from, strongest first.
	Interests []PreviewInterest `json:"interests"`
	Posts     []PreviewPost     `json:"posts"`
	TookMs    int64             `json:"tookMs"`
}

func scores(m map[string]float32) map[string]float64 {
	out := make(map[string]float64, len(m))
	for k, v := range m {
		out[k] = round3(v)
	}
	return out
}

// offers works out which topics can be added: every subtopic but the adult ones (as in the feed
// builder without adult access) and "unclear", which has none.
func (m *MeAPI) offers() ([]TopicChoice, map[string]bool) {
	m.offerOnce.Do(func() {
		m.offeredBy = map[string]bool{}
		for path, n := range m.Interests.Names {
			if !strings.Contains(path, "/") || strings.HasPrefix(path, "adult_content/") {
				continue
			}
			m.offered = append(m.offered, TopicChoice{Path: path, Name: n.Name, Broad: n.Broad})
			m.offeredBy[path] = true
		}
		slices.SortFunc(m.offered, func(a, b TopicChoice) int {
			if c := strings.Compare(a.Broad, b.Broad); c != 0 {
				return c
			}
			if c := strings.Compare(a.Name, b.Name); c != 0 {
				return c
			}
			return strings.Compare(a.Path, b.Path)
		})
	})
	return m.offered, m.offeredBy
}

// viewerTuning reads the viewer's saved tuning. One that isn't valid counts as none, as it does
// for their feed, so the page shows what the feed does.
func (m *MeAPI) viewerTuning(ctx context.Context, did string) (Tuning, error) {
	t, err := m.Tunings.ViewerTuning(ctx, did)
	if err != nil {
		return Tuning{}, err
	}
	if err := t.check(); err != nil {
		m.Log.Error("me: a viewer's saved tuning is not valid, ignoring it", "viewer", did, "err", err)
		return Tuning{}, nil
	}
	return t, nil
}

func (m *MeAPI) limited(w http.ResponseWriter, l *IPLimiter, did string) bool {
	if l == nil || l.Allow(did) {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(2))
	meJSON(w, http.StatusTooManyRequests, map[string]string{"error": "limited"})
	return true
}

func failure(w http.ResponseWriter, status int, code, message string) {
	body := map[string]string{"error": code}
	if message != "" {
		body["message"] = message
	}
	meJSON(w, status, body)
}

// ServeTuning answers GET /api/me/tuning: the signed-in viewer's saved tuning, and what the
// page needs to offer controls for it.
func (m *MeAPI) ServeTuning(w http.ResponseWriter, r *http.Request) {
	did, ok := m.signedIn(w, r)
	if !ok || m.limited(w, m.Edits, did) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), readTimeout)
	defer cancel()
	t, err := m.viewerTuning(ctx, did)
	if err != nil {
		m.Log.Error("me: reading a viewer's tuning failed", "viewer", did, "err", err)
		failure(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	offered, _ := m.offers()
	cfg := m.Interests.Cfg
	gap := 0
	if cfg.AuthorGap != nil {
		gap = *cfg.AuthorGap
	}
	minEngagement := 0.0
	if cfg.MinEngagement != nil {
		minEngagement = *cfg.MinEngagement
	}
	rankings := map[string]RankingValues{}
	for _, f := range []string{FreshnessPopular, FreshnessBalanced, FreshnessFresh} {
		r := Tuning{Freshness: f}.RankingFor(m.Ranking)
		rankings[f] = RankingValues{Gravity: r.Gravity, FreshEvery: r.FreshEvery, PromoPenalty: r.PromoPenalty,
			Like: r.Weights.Like, Repost: r.Weights.Repost, Reply: r.Weights.Reply, Quote: r.Weights.Quote}
	}
	meJSON(w, http.StatusOK, TuningResponse{
		Tuning: t,
		Defaults: TuningDefaults{Freshness: FreshnessBalanced, AuthorGap: gap, HalfLifeDays: cfg.HalfLifeDays, LookbackDays: cfg.LookbackDays,
			MinLikes: cfg.MinLikes, Interests: cfg.Topics, WindowHours: cfg.WindowHours, MinTopicProb: round3(cfg.MinTopicProb),
			MaxServes: cfg.MaxServes, ListSize: cfg.ListSize, MinEngagement: minEngagement, Ranking: rankings},
		Limits: TuningLimits{Weight: MaxTopicWeight, Boost: MaxBoost, Gravity: MaxGravity, FreshEvery: MaxFreshEvery, PromoPenalty: MaxPromoPenalty,
			Engagement: MaxEngagement, AuthorGap: MaxAuthorGap, LookbackDays: MaxLookbackDays, MinLikes: MaxMinLikes, Interests: MaxInterests,
			WindowHours: cfg.WindowHours, MinTopicProb: round3(cfg.MinTopicProb), MaxServes: MaxServesSetting, ListSize: min(cfg.ListSize, MaxListSetting),
			MinEngagement: MaxMinEngagement},
		MaxWeight: MaxTopicWeight,
		Tones:     Tones,
		Signals:   Signals,
		Topics:    offered,
	})
}

// readTuningBody checks a request that sends a tuning, and reads it. It answers the request
// itself and returns false if there is anything wrong. The request must come from this
// site's own pages and be JSON (a browser won't send that cross-site without asking first),
// and anything the tuning doesn't know about is refused rather than ignored.
func (m *MeAPI) readTuningBody(w http.ResponseWriter, r *http.Request) (Tuning, bool) {
	if !signin.SameOrigin(r, m.Origin) {
		failure(w, http.StatusForbidden, "forbidden", "")
		return Tuning{}, false
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		failure(w, http.StatusUnsupportedMediaType, "unsupported", "send JSON")
		return Tuning{}, false
	}
	dec := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxTuningBody))
	dec.DisallowUnknownFields()
	var t Tuning
	err := dec.Decode(&t)
	if err == nil {
		// Nothing may follow the tuning but space.
		switch tok, extra := dec.Token(); {
		case errors.Is(extra, io.EOF):
		case extra != nil:
			err = extra
		default:
			err = fmt.Errorf("unexpected %v after the tuning", tok)
		}
	}
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			failure(w, http.StatusRequestEntityTooLarge, "too_large", "")
			return Tuning{}, false
		}
		failure(w, http.StatusBadRequest, "invalid", "that isn't a tuning")
		return Tuning{}, false
	}
	_, topics := m.offers()
	if err := t.Validate(topics); err != nil {
		failure(w, http.StatusBadRequest, "invalid", err.Error())
		return Tuning{}, false
	}
	return t, true
}

// SaveTuning answers PUT /api/me/tuning: the body is the signed-in viewer's whole tuning, which
// replaces the one they had and is applied to their feed at once.
func (m *MeAPI) SaveTuning(w http.ResponseWriter, r *http.Request) {
	did, ok := m.signedIn(w, r)
	if !ok {
		return
	}
	if m.Personal == nil {
		failure(w, http.StatusNotFound, "off", "")
		return
	}
	t, ok := m.readTuningBody(w, r)
	if !ok || m.limited(w, m.Edits, did) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), saveTimeout)
	defer cancel()
	if err := m.Personal.SetTuning(ctx, did, t); err != nil {
		m.Log.Error("me: saving a viewer's tuning failed", "viewer", did, "err", err)
		failure(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	meJSON(w, http.StatusOK, map[string]any{"tuning": t})
}

// ServePreview answers POST /api/me/preview: the body is a draft tuning, and the answer is the
// first posts it would put in the signed-in viewer's feed. Nothing is saved, and nothing is
// marked as seen.
func (m *MeAPI) ServePreview(w http.ResponseWriter, r *http.Request) {
	did, ok := m.signedIn(w, r)
	if !ok {
		return
	}
	if m.Personal == nil {
		failure(w, http.StatusNotFound, "off", "")
		return
	}
	draft, ok := m.readTuningBody(w, r)
	if !ok || m.limited(w, m.Previews, did) {
		return
	}
	start := time.Now()
	ctx, cancel := context.WithTimeout(r.Context(), previewTimeout)
	defer cancel()
	result, err := m.Personal.Preview(ctx, m.Feed, did, draft, previewPosts)
	switch {
	case errors.Is(err, errNotReady):
		failure(w, http.StatusServiceUnavailable, "loading", "")
		return
	case err != nil:
		m.Log.Error("me: previewing a viewer's draft tuning failed", "viewer", did, "err", err)
		failure(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	posts := result.Posts

	// What the posts say. The preview is still useful as links if this fails, so say so in the
	// log and show them without text.
	texts := map[string]string{}
	if m.Texts != nil && len(posts) > 0 {
		uris := make([]string, len(posts))
		for i, p := range posts {
			uris[i] = p.URI
		}
		tctx, tcancel := context.WithTimeout(ctx, textTimeout)
		got, err := m.Texts.PostTexts(tctx, uris)
		tcancel()
		if err != nil {
			m.Log.Warn("me: reading the text of previewed posts failed", "viewer", did, "err", err)
		} else {
			texts = got
		}
	}
	name := func(path string) TopicName {
		if n := m.Interests.Names[path]; n.Name != "" {
			return n
		}
		return TopicName{Name: path}
	}
	resp := PreviewResponse{State: result.State, Posts: make([]PreviewPost, 0, len(posts)), Interests: make([]PreviewInterest, 0, len(result.Interests))}
	for _, t := range result.Interests {
		n := name(t.Path)
		resp.Interests = append(resp.Interests, PreviewInterest{Path: t.Path, Name: n.Name, Broad: n.Broad, Share: t.Share})
	}
	for _, p := range posts {
		n := name(p.TopPath)
		top := make([]TopicProb, 0, len(p.TopPaths))
		for i, path := range p.TopPaths {
			if i < len(p.TopPs) {
				top = append(top, TopicProb{Path: path, Name: name(path).Name, P: round3(p.TopPs[i])})
			}
		}
		labels := p.Labels
		if labels == nil {
			labels = []string{}
		}
		resp.Posts = append(resp.Posts, PreviewPost{URI: p.URI, URL: postURL(p.URI), DID: p.DID, Text: cleanText(texts[p.URI]),
			Topic: n.Name, TopicPath: p.TopPath, Broad: n.Broad, Top: top, Tone: scores(p.Tone), Signals: scores(p.Signals), Labels: labels,
			Score: math.Round(p.Score*1000) / 1000, IndexedAt: p.IndexedAt.UTC(),
			Likes: p.Likes, Reposts: p.Reposts, Replies: p.Replies, Quotes: p.Quotes})
	}
	resp.TookMs = time.Since(start).Milliseconds()
	meJSON(w, http.StatusOK, resp)
}
