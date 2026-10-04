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

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/golang-jwt/jwt/v5"
	"github.com/jcalabro/atmos"
	atmosidentity "github.com/jcalabro/atmos/identity"
	"github.com/labstack/echo/v4"
	"github.com/labstack/echo/v4/middleware"

	"github.com/haileyok/topic-feed/internal/signin"
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
	audiences []string
	// dir finds the keys that viewers' credentials are signed with.
	dir  *atmosidentity.Directory
	echo *echo.Echo

	// Interactions stores what sendInteractions receives; nil answers 501.
	Interactions InteractionSink
	// Preview backs the feed builder page's API; nil answers 404.
	Preview *Previewer
	// Personal serves the feeds configured with `personal:`; nil leaves them empty.
	Personal *Personal
	// SignIn serves sign-in with Bluesky (/oauth/... and /api/me); nil answers 404.
	SignIn *signin.Handler
	// Me serves a signed-in viewer's own data (/api/me/...); nil answers 404.
	Me *MeAPI
	// Inspect serves the post inspector (/api/inspect) to the owner of the feeds; nil answers 404.
	Inspect *InspectAPI
	// FeedsAPI serves a signed-in account's own feeds (/api/me/feeds) and the client metadata of the
	// page that publishes them; nil answers 404.
	FeedsAPI *FeedsAPI
	// ResolveHandle answers the handle lookups of that page; nil answers 404.
	ResolveHandle *ResolveHandleAPI
}

// NewServer builds the HTTP server. dir resolves viewers' DIDs to the keys their credentials
// are checked with. Viewers' handles aren't needed for that, so it can skip verifying them.
//
// Credentials are checked with the atmos library (serviceauth), the same one that signs people
// in. Two libraries can't both check these JWTs in one program: each registers its own
// ES256 and ES256K signing methods in a table the JWT library shares, and the one that is
// registered last would silently break the other.
func NewServer(cfg ServerConfig, feeds *Feeds, dir *atmosidentity.Directory, log *slog.Logger) *Server {
	s := &Server{cfg: cfg, feeds: feeds, log: log, dir: dir,
		audiences: []string{cfg.ServiceDID, cfg.ServiceDID + "#bsky_fg"}}
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
	e.GET("/adult-access", func(c echo.Context) error {
		if s.Preview == nil {
			return echo.ErrNotFound
		}
		return s.Preview.HandleAdultAccess(c)
	})
	e.GET("/oauth/client-metadata.json", s.signInRoute((*signin.Handler).ServeMetadata))
	e.POST("/oauth/login", s.signInRoute((*signin.Handler).ServeLogin))
	e.GET("/oauth/callback", s.signInRoute((*signin.Handler).ServeCallback))
	e.POST("/oauth/logout", s.signInRoute((*signin.Handler).ServeLogout))
	e.GET("/api/me", s.signInRoute((*signin.Handler).ServeMe))
	e.GET("/api/me/interests", s.meRoute((*MeAPI).ServeInterests))
	e.GET("/api/me/tuning", s.meRoute((*MeAPI).ServeTuning))
	e.PUT("/api/me/tuning", s.meRoute((*MeAPI).SaveTuning))
	e.POST("/api/me/preview", s.meRoute((*MeAPI).ServePreview))
	e.GET("/api/inspect", s.inspectRoute((*InspectAPI).ServeInspect))
	e.GET("/api/me/feeds", s.feedsRoute((*FeedsAPI).ServeList))
	e.PUT("/api/me/feeds/:rkey", s.feedsRoute((*FeedsAPI).ServeSave))
	e.DELETE("/api/me/feeds/:rkey", s.feedsRoute((*FeedsAPI).ServeDelete))
	e.GET("/oauth/browser-client-metadata.json", s.feedsRoute((*FeedsAPI).ServeClientMetadata))
	e.GET("/xrpc/com.atproto.identity.resolveHandle", func(c echo.Context) error {
		if s.ResolveHandle == nil {
			return echo.ErrNotFound
		}
		s.ResolveHandle.ServeResolve(c.Response(), c.Request())
		return nil
	})
	addWebRoutes(e, "https://"+cfg.Hostname)
	s.echo = e
	return s
}

// signInRoute serves one of the sign-in handler's routes, or 404 while sign-in is off.
func (s *Server) signInRoute(serve func(*signin.Handler, http.ResponseWriter, *http.Request)) echo.HandlerFunc {
	return func(c echo.Context) error {
		if s.SignIn == nil {
			return echo.ErrNotFound
		}
		serve(s.SignIn, c.Response(), c.Request())
		return nil
	}
}

// meRoute serves one of the signed-in viewer's routes, or 404 while they are off.
func (s *Server) meRoute(serve func(*MeAPI, http.ResponseWriter, *http.Request)) echo.HandlerFunc {
	return func(c echo.Context) error {
		if s.Me == nil {
			return echo.ErrNotFound
		}
		serve(s.Me, c.Response(), c.Request())
		return nil
	}
}

// inspectRoute serves the post inspector, or 404 while it is off.
func (s *Server) inspectRoute(serve func(*InspectAPI, http.ResponseWriter, *http.Request)) echo.HandlerFunc {
	return func(c echo.Context) error {
		if s.Inspect == nil {
			return echo.ErrNotFound
		}
		serve(s.Inspect, c.Response(), c.Request())
		return nil
	}
}

// feedsRoute serves one of the routes for people's own feeds, or 404 while they are off. The rkey in the
// address is handed on as a path value of the request.
func (s *Server) feedsRoute(serve func(*FeedsAPI, http.ResponseWriter, *http.Request)) echo.HandlerFunc {
	return func(c echo.Context) error {
		if s.FeedsAPI == nil {
			return echo.ErrNotFound
		}
		r := c.Request()
		if rkey := c.Param("rkey"); rkey != "" {
			r.SetPathValue("rkey", rkey)
		}
		serve(s.FeedsAPI, c.Response(), r)
		return nil
	}
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
	key, ok := s.feedKey(c.QueryParam("feed"))
	if !ok {
		return fail(http.StatusBadRequest, "unknown", "UnknownFeed", "unknown feed")
	}
	if _, _, ok := s.feeds.Posts(key); !ok {
		return fail(http.StatusBadRequest, "unknown", "UnknownFeed", "unknown feed")
	}
	label := feedLabel(key)
	limit := defaultLimit
	if v := c.QueryParam("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > maxLimit {
			return fail(http.StatusBadRequest, label, "InvalidRequest", "limit must be 1-100")
		}
		limit = n
	}
	if s.Personal != nil && s.Personal.Serves(key) {
		return s.personalSkeleton(c, key, limit, fail)
	}
	items, next, ready, err := s.feeds.Page(c.Request().Context(), key, c.QueryParam("cursor"), limit)
	if err != nil {
		return fail(http.StatusBadRequest, label, "InvalidRequest", err.Error())
	}
	if !ready {
		return fail(http.StatusServiceUnavailable, label, "NotReady", "feed is still loading")
	}
	if viewer := s.viewer(c); viewer != "" {
		s.log.Debug("skeleton", "feed", key, "viewer", viewer, "posts", len(items))
	}
	resp := skeletonResponse{Feed: make([]skeletonItem, len(items)), Cursor: next, ReqID: newReqID(label)}
	for i, it := range items {
		resp.Feed[i] = skeletonItem{Post: it.URI, FeedContext: it.Context}
	}
	metricRequests.WithLabelValues(label, "200").Inc()
	return c.JSON(http.StatusOK, resp)
}

// feedLabel names a feed in metrics and in request IDs. The service owner's feeds are named by their
// rkey; everyone else's share one name, since there is no limit to how many there can be (and a key
// with a DID in it would not fit the rkey-dash-hex form of a request ID).
func feedLabel(key string) string {
	if strings.Contains(key, "/") {
		return "user"
	}
	return key
}

// personalSkeleton answers a request for a personal feed: the viewer's own feed, or the
// welcome post while it is being built or when there is no viewer.
func (s *Server) personalSkeleton(c echo.Context, rkey string, limit int, fail func(int, string, string, string) error) error {
	page, err := s.Personal.Page(c.Request().Context(), rkey, s.viewer(c), c.QueryParam("cursor"), limit)
	switch {
	case errors.Is(err, errBadCursor):
		return fail(http.StatusBadRequest, rkey, "InvalidRequest", err.Error())
	case errors.Is(err, errNotReady):
		return fail(http.StatusServiceUnavailable, rkey, "NotReady", "feed is still loading")
	case err != nil:
		s.log.Warn("personal skeleton", "feed", rkey, "err", err)
		return fail(http.StatusInternalServerError, rkey, "InternalServerError", "could not build the feed")
	}
	resp := skeletonResponse{Feed: make([]skeletonItem, len(page.Items)), Cursor: page.Cursor, ReqID: newReqID(rkey)}
	for i, it := range page.Items {
		resp.Feed[i] = skeletonItem{Post: it.URI, FeedContext: it.Context}
	}
	metricPersonalPages.WithLabelValues(rkey, page.State).Inc()
	metricRequests.WithLabelValues(rkey, "200").Inc()
	return c.JSON(http.StatusOK, resp)
}

// feedKey returns the key (Feed.Key) of the feed a feed URI names: a generator record in anyone's repo,
// which is the rkey alone when the repo is the service owner's. Whether there is such a feed is for the
// caller to see.
func (s *Server) feedKey(feed string) (string, bool) {
	u, err := syntax.ParseATURI(feed)
	if err != nil {
		return "", false
	}
	did, err := u.Authority().AsDID()
	if err != nil || u.Collection().String() != generatorCollection {
		return "", false
	}
	if did == s.cfg.OwnerDID {
		return u.RecordKey().String(), true
	}
	return did.String() + "/" + u.RecordKey().String(), true
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
	for _, aud := range s.audiences {
		var did atmos.DID
		did, err = verifyCredential(ctx, token, aud, atmos.NSID(method.String()), s.dir)
		if err == nil {
			return string(did), nil
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
	// Only a feed we serve is named: what anyone sends must not make up new metric labels or rows.
	feed, _ := s.feedKey(in.Feed)
	if _, _, known := s.feeds.Posts(feed); !known {
		feed = ""
	}
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
		metricInteractions.WithLabelValues(orUnknown(feedLabel(f)), knownEvent(ev)).Inc()
	}
	s.Interactions.Add(rows)
	if s.Personal != nil {
		// Whatever the viewer met in any of our feeds is not shown to them again.
		met := make([]string, len(rows))
		for i, r := range rows {
			met[i] = r.Item
		}
		s.Personal.NoteInteractions(viewer, met)
	}
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
	// The owner's feeds, and some of everyone else's: there is no limit to how many people make.
	owners := s.feeds.List()
	others := s.feeds.UserFeeds(maxDescribedUserFeeds)
	feeds := make([]feed, 0, len(owners)+len(others))
	for _, f := range owners {
		feeds = append(feeds, feed{URI: s.FeedURI(f.Rkey)})
	}
	for _, f := range others {
		feeds = append(feeds, feed{URI: "at://" + f.Owner + "/" + generatorCollection + "/" + f.Rkey})
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
		built := posts != nil
		if f.Personal != nil { // the posts its viewers' feeds draw on
			var n int
			n, builtAt, built = 0, time.Time{}, false
			if s.Personal != nil {
				n, builtAt, built = s.Personal.Status(f.Rkey)
			}
			st.Posts = n
		}
		if built {
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
		// Adult feeds aren't listed, and a personal feed has no settings to remix.
		if f.AllowAdult || f.Personal != nil {
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
