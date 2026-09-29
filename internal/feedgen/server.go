package feedgen

import (
	"context"
	"errors"
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

var skeletonMethod = syntax.NSID("app.bsky.feed.getFeedSkeleton")

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
	e.GET("/.well-known/did.json", s.handleDIDDoc)
	e.GET("/healthz", s.handleHealth)
	e.GET("/", s.handleRoot)
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
	Post string `json:"post"`
}

type skeletonResponse struct {
	Feed   []skeletonItem `json:"feed"`
	Cursor string         `json:"cursor,omitempty"`
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
	posts, _, ok := s.feeds.Posts(rkey)
	if !ok {
		return fail(http.StatusBadRequest, "unknown", "UnknownFeed", "unknown feed")
	}
	if posts == nil {
		return fail(http.StatusServiceUnavailable, rkey, "NotReady", "feed is still loading")
	}
	limit := defaultLimit
	if v := c.QueryParam("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLimit {
			return fail(http.StatusBadRequest, rkey, "InvalidRequest", "limit must be 1-100")
		}
		limit = n
	}
	out, next, err := page(posts, c.QueryParam("cursor"), limit)
	if err != nil {
		return fail(http.StatusBadRequest, rkey, "InvalidRequest", err.Error())
	}
	if viewer := s.viewer(c); viewer != "" {
		s.log.Debug("skeleton", "feed", rkey, "viewer", viewer, "posts", len(out))
	}
	resp := skeletonResponse{Feed: make([]skeletonItem, len(out)), Cursor: next}
	for i, p := range out {
		resp.Feed[i] = skeletonItem{Post: p.URI}
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

// viewer returns the DID from a valid viewer credential, or "". Feeds aren't personalized,
// so a missing or invalid credential still gets the feed; the outcome is only counted.
func (s *Server) viewer(c echo.Context) string {
	token, ok := strings.CutPrefix(c.Request().Header.Get("Authorization"), "Bearer ")
	if !ok || token == "" {
		metricAuth.WithLabelValues("none").Inc()
		return ""
	}
	ctx, cancel := context.WithTimeout(c.Request().Context(), 3*time.Second)
	defer cancel()
	var err error
	for _, v := range s.validators {
		var did syntax.DID
		did, err = v.Validate(ctx, token, &skeletonMethod)
		if err == nil {
			metricAuth.WithLabelValues("ok").Inc()
			return did.String()
		}
		if !errors.Is(err, jwt.ErrTokenInvalidAudience) {
			break
		}
	}
	metricAuth.WithLabelValues("invalid").Inc()
	s.log.Debug("invalid viewer credential", "err", err)
	return ""
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

func (s *Server) handleRoot(c echo.Context) error {
	var b strings.Builder
	b.WriteString("Topic feed generator " + s.cfg.ServiceDID + "\n\n")
	for _, f := range s.feeds.List() {
		b.WriteString(f.DisplayName + "  " + s.FeedURI(f.Rkey) + "\n")
	}
	return c.String(http.StatusOK, b.String())
}
