package feedgen

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/auth"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/golang-jwt/jwt/v5"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"
)

const (
	generatorCollection = "app.bsky.feed.generator"
	defaultLimit        = 50
	maxLimit            = 100
)

var (
	skeletonMethod     = syntax.NSID("app.bsky.feed.getFeedSkeleton")
	interactionsMethod = syntax.NSID("app.bsky.feed.sendInteractions")
)

const (
	maxInteractionsBody = 1 << 20 // bytes per sendInteractions call
	maxInteractions     = 1000    // interactions per call
)

// ServerConfig identifies the service on the network.
type ServerConfig struct {
	Hostname   string     // public hostname, e.g. feeds.example.com (served over HTTPS)
	ServiceDID string     // the feed generator's DID, usually did:web:<Hostname>
	OwnerDID   syntax.DID // account whose repo holds the app.bsky.feed.generator records
	// MaxAge is how old a feed's posts may get before /healthz reports it unhealthy.
	MaxAge time.Duration
}

// Server answers the feed generator endpoints from Feeds.
type Server struct {
	cfg   ServerConfig
	feeds *Feeds
	log   *slog.Logger
	// Viewer credentials may name the service DID with or without the #bsky_fg fragment.
	validators []*auth.ServiceAuthValidator
	echo       *echo.Echo

	// Interactions stores what sendInteractions receives; nil answers 501.
	Interactions InteractionSink
	// Preview backs the feed builder page's API; nil answers 404.
	Preview *Previewer
}

// NewServer builds the HTTP server. dir resolves viewers' DIDs to check their credentials.
func NewServer(cfg ServerConfig, feeds *Feeds, dir identity.Directory, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, feeds: feeds, log: log}
	for _, aud := range []string{cfg.ServiceDID, cfg.ServiceDID + "#bsky_fg"} {
		s.validators = append(s.validators, &auth.ServiceAuthValidator{Audience: aud, Dir: dir, TimestampLeeway: 30 * time.Second})
	}
	e := echo.New()
	e.HideBanner, e.HidePort = true, true
	e.Use(middleware.Recover(), s.observe)
	e.GET("/xrpc/app.bsky.feed.getFeedSkeleton", s.handleSkeleton)
	e.GET("/xrpc/app.bsky.feed.describeFeedGenerator", s.handleDescribe)
	e.POST("/xrpc/app.bsky.feed.sendInteractions", s.handleInteractions)
	e.GET("/.well-known/did.json", s.handleDIDDoc)
	e.GET("/healthz", s.handleHealth)
	e.GET("/api/feeds", s.handleFeeds)
	e.GET("/api/taxonomy", func(c echo.Context) error {
		if s.Preview == nil {
			return echo.ErrNotFound
		}
		return s.Preview.HandleTaxonomy(c)
	})
	e.POST("/api/preview", func(c echo.Context) error {
		if s.Preview == nil {
			return echo.ErrNotFound
		}
		return s.Preview.Handle(c)
	})
	addWebRoutes(e)
	s.echo = e
	return s
}

// Start serves on addr until Shutdown.
func (s *Server) Start(addr string) error {
	err := s.echo.Start(addr)
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return err
}

// Shutdown stops accepting requests and waits for in-flight ones.
func (s *Server) Shutdown(ctx context.Context) error { return s.echo.Shutdown(ctx) }

// Handler exposes the routes (for tests).
func (s *Server) Handler() http.Handler { return s.echo }

// FeedURI is the at:// URI of a feed's generator record.
func (s *Server) FeedURI(rkey string) string {
	return "at://" + s.cfg.OwnerDID.String() + "/" + generatorCollection + "/" + rkey
}

// observe records request durations by route and logs server errors and slow requests.
func (s *Server) observe(next echo.HandlerFunc) echo.HandlerFunc {
	return func(c echo.Context) error {
		start := time.Now()
		err := next(c)
		if err != nil {
			c.Error(err)
		}
		took := time.Since(start)
		route := c.Path()
		if route == "" {
			route = "unmatched"
		}
		metricRequestSeconds.WithLabelValues(route).Observe(took.Seconds())
		if status := c.Response().Status; status >= 500 || took > time.Second {
			s.log.Warn("request", "route", route, "status", status, "took", took.Round(time.Millisecond), "err", err)
		}
		return nil
	}
}

type xrpcError struct {
	Error   string `json:"error"`
	Message string `json:"message,omitempty"`
}

type skeletonItem struct {
	Post        string `json:"post"`
	FeedContext string `json:"feedContext,omitempty"`
}

type skeletonResponse struct {
	Feed   []skeletonItem `json:"feed"`
	Cursor string         `json:"cursor,omitempty"`
	ReqID  string         `json:"reqId,omitempty"` // passed back alongside interactions
}

func (s *Server) handleSkeleton(c echo.Context) error {
	fail := func(status int, feed, name, msg string) error {
		metricRequests.WithLabelValues(feed, strconv.Itoa(status)).Inc()
		return c.JSON(status, xrpcError{Error: name, Message: msg})
	}
	rkey, ok := s.feedRkey(c.QueryParam("feed"))
	if !ok {
		return fail(http.StatusBadRequest, "unknown", "UnknownFeed", "unknown feed")
	}
	if _, _, ok := s.feeds.Posts(rkey); !ok {
		return fail(http.StatusBadRequest, "unknown", "UnknownFeed", "unknown feed")
	}
	limit := defaultLimit
	if v := c.QueryParam("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLimit {
			return fail(http.StatusBadRequest, rkey, "InvalidRequest", "limit must be 1-100")
		}
		limit = n
	}
	items, next, ready, err := s.feeds.Page(rkey, c.QueryParam("cursor"), limit)
	if err != nil {
		return fail(http.StatusBadRequest, rkey, "InvalidRequest", err.Error())
	}
	if !ready {
		return fail(http.StatusServiceUnavailable, rkey, "NotReady", "feed is still loading")
	}
	if viewer := s.viewer(c); viewer != "" {
		s.log.Debug("skeleton", "feed", rkey, "viewer", viewer, "posts", len(items))
	}
	resp := skeletonResponse{Feed: make([]skeletonItem, len(items)), Cursor: next, ReqID: newReqID(rkey)}
	for i, it := range items {
		resp.Feed[i] = skeletonItem{Post: it.URI, FeedContext: it.Context}
	}
	metricRequests.WithLabelValues(rkey, "200").Inc()
	return c.JSON(http.StatusOK, resp)
}

// feedRkey returns the rkey of a feed URI that names one of this owner's generator records.
func (s *Server) feedRkey(feed string) (string, bool) {
	u, err := syntax.ParseATURI(feed)
	if err != nil {
		return "", false
	}
	did, err := u.Authority().AsDID()
	if err != nil || did != s.cfg.OwnerDID || u.Collection().String() != generatorCollection {
		return "", false
	}
	return u.RecordKey().String(), true
}

var errNoCredential = errors.New("no credential")

// authenticate returns the DID from the request's service credential for method.
func (s *Server) authenticate(c echo.Context, method syntax.NSID) (string, error) {
	token, ok := strings.CutPrefix(c.Request().Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		return "", errNoCredential
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 3*time.Second)
	defer cancel()
	var err error
	for _, v := range s.validators {
		var did syntax.DID
		did, err = v.Validate(ctx, token, &method)
		if err == nil {
			return did.String(), nil
		}
		if !errors.Is(err, jwt.ErrTokenInvalidAudience) {
			break
		}
	}
	return "", err
}

// viewer returns the DID from a valid viewer credential, or "". Feeds aren't personalized,
// so a missing or invalid credential still gets the feed; the outcome is only counted.
func (s *Server) viewer(c echo.Context) string {
	did, err := s.authenticate(c, skeletonMethod)
	switch {
	case err == nil:
		metricAuth.WithLabelValues("ok").Inc()
	case errors.Is(err, errNoCredential):
		metricAuth.WithLabelValues("none").Inc()
	default:
		metricAuth.WithLabelValues("invalid").Inc()
		s.log.Debug("invalid viewer credential", "err", err)
	}
	return did
}

type interactionsRequest struct {
	Feed         string `json:"feed"`
	Interactions []struct {
		Item        string `json:"item"`
		Event       string `json:"event"`
		FeedContext string `json:"feedContext"`
		ReqID       string `json:"reqId"`
	} `json:"interactions"`
}

// handleInteractions stores interactions with our feeds' posts, with the viewer's DID.
// Unlike feed requests, these need a valid credential: without one there is no viewer.
func (s *Server) handleInteractions(c echo.Context) error {
	fail := func(status int, name, msg string) error {
		metricInteractionRequests.WithLabelValues(strconv.Itoa(status)).Inc()
		return c.JSON(status, xrpcError{Error: name, Message: msg})
	}
	if s.Interactions == nil {
		return fail(http.StatusNotImplemented, "MethodNotImplemented", "interactions are not recorded")
	}
	viewer, err := s.authenticate(c, interactionsMethod)
	if err != nil {
		return fail(http.StatusUnauthorized, "AuthRequired", "a valid service credential is required")
	}
	var in interactionsRequest
	if err := json.NewDecoder(http.MaxBytesReader(c.Response(), c.Request().Body, maxInteractionsBody)).Decode(&in); err != nil {
		return fail(http.StatusBadRequest, "InvalidRequest", "body must be JSON {interactions: [...]}")
	}
	if len(in.Interactions) > maxInteractions {
		return fail(http.StatusBadRequest, "InvalidRequest", fmt.Sprintf("at most %d interactions per call", maxInteractions))
	}
	feed, _ := s.feedRkey(in.Feed)
	now := time.Now().UTC()
	rows := make([]InteractionRow, 0, len(in.Interactions))
	for _, it := range in.Interactions {
		if it.Item == "" || it.Event == "" {
			continue
		}
		f := feed
		if f == "" { // older clients don't send feed: our reqIds start with the feed's rkey
			if rk, _, ok := strings.Cut(it.ReqID, "-"); ok {
				if _, _, known := s.feeds.Posts(rk); known {
					f = rk
				}
			}
		}
		ev := strings.TrimPrefix(it.Event, "app.bsky.feed.defs#")
		rows = append(rows, InteractionRow{ReceivedAt: now, ViewerDID: viewer, Feed: f,
			Item: clip(it.Item, 512), Event: clip(ev, 100), FeedContext: clip(it.FeedContext, 2000), ReqID: clip(it.ReqID, 100)})
		metricInteractions.WithLabelValues(orUnknown(f), knownEvent(ev)).Inc()
	}
	s.Interactions.Add(rows)
	metricInteractionRequests.WithLabelValues("200").Inc()
	return c.JSON(http.StatusOK, map[string]any{})
}

func clip(s string, n int) string {
	if len(s) > n {
		return s[:n]
	}
	return s
}

func orUnknown(s string) string {
	if s == "" {
		return "unknown"
	}
	return s
}

// knownEvent keeps metric labels bounded: unrecognized event names count as "other".
func knownEvent(ev string) string {
	switch ev {
	case "requestLess", "requestMore", "clickthroughItem", "clickthroughAuthor", "clickthroughReposter",
		"clickthroughEmbed", "interactionSeen", "interactionLike", "interactionRepost", "interactionReply",
		"interactionQuote", "interactionShare":
		return ev
	}
	return "other"
}

// newReqID returns a request ID: the feed's rkey, a dash, and 32 random hex characters.
func newReqID(rkey string) string {
	b := make([]byte, 16)
	rand.Read(b)
	return rkey + "-" + hex.EncodeToString(b)
}

func (s *Server) handleDescribe(c echo.Context) error {
	type feed struct {
		URI string `json:"uri"`
	}
	feeds := make([]feed, 0, len(s.feeds.List()))
	for _, f := range s.feeds.List() {
		feeds = append(feeds, feed{URI: s.FeedURI(f.Rkey)})
	}
	return c.JSON(http.StatusOK, map[string]any{"did": s.cfg.ServiceDID, "feeds": feeds})
}

// handleDIDDoc serves the did:web document when the service DID is did:web:<Hostname>.
func (s *Server) handleDIDDoc(c echo.Context) error {
	if s.cfg.ServiceDID != "did:web:"+s.cfg.Hostname {
		return c.JSON(http.StatusNotFound, xrpcError{Error: "NotFound", Message: "service DID is not a did:web for this host"})
	}
	return c.JSON(http.StatusOK, map[string]any{
		"@context": []string{"https://www.w3.org/ns/did/v1"},
		"id":       s.cfg.ServiceDID,
		"service": []map[string]string{{
			"id":              "#bsky_fg",
			"type":            "BskyFeedGenerator",
			"serviceEndpoint": "https://" + s.cfg.Hostname,
		}},
	})
}

func (s *Server) handleHealth(c echo.Context) error {
	type status struct {
		Posts      int     `json:"posts"`
		AgeSeconds float64 `json:"age_seconds"`
		OK         bool    `json:"ok"`
	}
	all := map[string]status{}
	healthy := true
	for _, f := range s.feeds.List() {
		posts, builtAt, _ := s.feeds.Posts(f.Rkey)
		st := status{Posts: len(posts), AgeSeconds: -1}
		if posts != nil {
			st.AgeSeconds = time.Since(builtAt).Round(time.Second).Seconds()
			st.OK = time.Since(builtAt) <= s.cfg.MaxAge
		}
		healthy = healthy && st.OK
		all[f.Rkey] = st
	}
	code := http.StatusOK
	if !healthy {
		code = http.StatusServiceUnavailable
	}
	return c.JSON(code, map[string]any{"ok": healthy, "feeds": all})
}

type publishedFeed struct {
	Rkey        string             `json:"rkey"`
	DisplayName string             `json:"display_name"`
	Description string             `json:"description"`
	URI         string             `json:"uri"`
	URL         string             `json:"url"` // the feed in the Bluesky app
	Posts       int                `json:"posts"`
	Paths       []string           `json:"paths"`
	MinProb     float32            `json:"min_prob"`
	Exclude     map[string]float32 `json:"exclude,omitempty"`
	Tone        Rules              `json:"tone"`
	Signals     Rules              `json:"signals"`
	Ranking     Ranking            `json:"ranking"`
}

// handleFeeds answers GET /api/feeds: every served feed with its settings, for the
// directory and "remix" on the builder page.
func (s *Server) handleFeeds(c echo.Context) error {
	out := []publishedFeed{}
	for _, f := range s.feeds.List() {
		if f.AllowAdult {
			continue
		}
		posts, _, _ := s.feeds.Posts(f.Rkey)
		out = append(out, publishedFeed{Rkey: f.Rkey, DisplayName: f.DisplayName, Description: f.Description,
			URI: s.FeedURI(f.Rkey), URL: "https://bsky.app/profile/" + s.cfg.OwnerDID.String() + "/feed/" + f.Rkey,
			Posts: len(posts), Paths: f.Paths, MinProb: f.MinProb, Exclude: f.Exclude, Tone: f.Tone,
			Signals: f.Signals, Ranking: f.Ranking})
	}
	c.Response().Header().Set("Cache-Control", "public, max-age=30")
	return c.JSON(http.StatusOK, out)
}
