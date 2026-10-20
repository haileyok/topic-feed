package signin

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"log/slog"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/jcalabro/atmos/oauth"
)

const (
	// loginTimeout bounds how long starting a login may take: it asks the person's own
	// server, which can be slow or hostile.
	loginTimeout = 20 * time.Second
	// maxAccountLen is longer than any handle (253) or DID the form could hold.
	maxAccountLen = 300
	maxFormBytes  = 4 << 10
)

// Config is what the sign-in routes need.
type Config struct {
	// Origin is where the service is served from, like https://feeds.example.com.
	Origin string
	// Auth signs people in.
	Auth Authenticator
	// Metadata is the OAuth client metadata served at /oauth/client-metadata.json.
	Metadata oauth.ClientMetadata
	// Sessions issues and checks the cookie.
	Sessions *Sessions
	// Handle looks up the handle of a DID for /api/me; nil, or "" back: the DID is all it says.
	Handle func(ctx context.Context, did string) string
	// Owner is the DID of the account that runs the service. /api/me says "owner": true when it
	// is the one signed in, so the pages can offer what only the owner may use. "" for nobody.
	Owner string
	// Allow, if set, is asked before a login starts; false answers 429. Use it to limit how
	// often one visitor can make this service call out to other servers.
	Allow func(r *http.Request) bool
	// Home is where people land after signing in or out, and where sign-in problems are
	// reported (as ?signin=denied, invalid, busy, or failed). Default /me.
	Home string
	// Returns are the pages a login may come back to instead of Home: the sign-in form sends the
	// path of the page it is on (the form field "return"), and only a path listed here exactly is
	// used, so a link can't send someone elsewhere after they sign in.
	Returns []string
	// Connect, if set, is the sign-in for the filtered feeds (/oauth/connect...), which keeps the
	// viewer's sign-in to ask their server for tokens; nil: those routes answer 404.
	Connect *Connector
	Log     *slog.Logger
}

// Handler serves the sign-in routes and tells the rest of the service who is signed in.
type Handler struct {
	cfg        Config
	secure     bool
	sessionKey string // cookie names
	stateKey   string
	returnKey  string
}

// New checks cfg and prepares the routes.
func New(cfg Config) (*Handler, error) {
	if err := checkOrigin(cfg.Origin); err != nil {
		return nil, err
	}
	if cfg.Auth == nil || cfg.Sessions == nil {
		return nil, errors.New("sign-in needs an authenticator and sessions")
	}
	if cfg.Home == "" {
		cfg.Home = "/me"
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	h := &Handler{cfg: cfg, secure: strings.HasPrefix(cfg.Origin, "https://")}
	// The __Host- prefix makes a browser refuse these unless they were set over https for
	// this exact host, and keeps other subdomains from planting their own.
	h.sessionKey, h.stateKey, h.returnKey = "feeds_session", "feeds_login", "feeds_return"
	if h.secure {
		h.sessionKey, h.stateKey, h.returnKey = "__Host-feeds_session", "__Host-feeds_login", "__Host-feeds_return"
	}
	return h, nil
}

// returnCookie remembers, while a login is under way, the page it should come back to. It is set
// and cleared together with the state cookie, and holds only a path from cfg.Returns.
func (h *Handler) returnCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: h.returnKey, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteLaxMode}
}

// allowedReturn is path when it is one of cfg.Returns, else "".
func (h *Handler) allowedReturn(path string) string {
	if path != "" && slices.Contains(h.cfg.Returns, path) {
		return path
	}
	return ""
}

// landing is where this request's login comes back to: the page it started from, if it was one
// of cfg.Returns, else Home.
func (h *Handler) landing(r *http.Request) string {
	if c, err := r.Cookie(h.returnKey); err == nil {
		if to := h.allowedReturn(c.Value); to != "" {
			return to
		}
	}
	return h.cfg.Home
}

// stateCookie is the cookie that ties a login to the browser that started it (an empty value
// and a negative maxAge clear it). Its path is "/" because it has the __Host- prefix, which
// stops other subdomains from planting a cookie of the same name, and browsers keep a
// __Host- cookie only if its path is "/": they silently discard any other, which is a login
// that can never finish. Setting and clearing must agree, so both come from here.
func (h *Handler) stateCookie(value string, maxAge int) *http.Cookie {
	return &http.Cookie{Name: h.stateKey, Value: value, Path: "/", MaxAge: maxAge,
		HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteLaxMode}
}

// Origin is where the service is served from.
func (h *Handler) Origin() string { return h.cfg.Origin }

// Mount registers the routes on mux.
func (h *Handler) Mount(mux *http.ServeMux) {
	mux.HandleFunc("GET /oauth/client-metadata.json", h.ServeMetadata)
	mux.HandleFunc("POST /oauth/login", h.ServeLogin)
	mux.HandleFunc("GET /oauth/callback", h.ServeCallback)
	mux.HandleFunc("POST /oauth/logout", h.ServeLogout)
	mux.HandleFunc("GET /api/me", h.ServeMe)
}

// Viewer is the account signed in on this request: it comes from a cookie only this service
// can have issued, never from anything the request says.
func (h *Handler) Viewer(r *http.Request) (did string, ok bool) {
	c, err := r.Cookie(h.sessionKey)
	if err != nil {
		return "", false
	}
	return h.cfg.Sessions.Verify(c.Value)
}

// SameOrigin reports whether a state-changing request came from this service's own pages.
// Browsers say where a request comes from: Origin when they send one, else Sec-Fetch-Site.
// Requests from tools that send neither can't be somebody else's page.
func SameOrigin(r *http.Request, origin string) bool {
	if o := r.Header.Get("Origin"); o != "" {
		return o == origin
	}
	if s := r.Header.Get("Sec-Fetch-Site"); s != "" {
		return s == "same-origin" || s == "none"
	}
	return true
}

func noStore(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
}

// ServeMetadata serves the OAuth client metadata: the document at the client ID.
func (h *Handler) ServeMetadata(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(h.cfg.Metadata)
}

// home redirects to the page the login started from (see landing), or the home page, reporting a
// problem if there was one.
func (h *Handler) home(w http.ResponseWriter, r *http.Request, problem string) {
	to := h.landing(r)
	if problem != "" {
		to += "?signin=" + url.QueryEscape(problem)
	}
	http.Redirect(w, r, to, http.StatusSeeOther)
}

// wantsJSON reports whether the caller is the page's own script, which asks for JSON: it
// can't follow a redirect to another site (the page's content security policy forbids
// that for forms, and a fetch can't read where it leads), so it is told where to go.
func wantsJSON(r *http.Request) bool {
	return strings.Contains(r.Header.Get("Accept"), "application/json")
}

func replyJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// problem reports why a login didn't go ahead, as the code the page turns into words: as
// JSON to the page's script, else as a redirect to the home page.
func (h *Handler) problem(w http.ResponseWriter, r *http.Request, code string, status int) {
	if wantsJSON(r) {
		replyJSON(w, status, map[string]string{"error": code})
		return
	}
	h.home(w, r, code)
}

// normalizeAccount tidies what someone typed into the sign-in box: surrounding space, a
// leading @, and a pasted profile link are all things people do.
func normalizeAccount(s string) string {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "@")
	for _, prefix := range []string{"https://bsky.app/profile/", "http://bsky.app/profile/"} {
		if rest, ok := strings.CutPrefix(s, prefix); ok {
			s, _, _ = strings.Cut(rest, "/")
		}
	}
	return strings.ToLower(s)
}

// ServeLogin starts signing in the account in the form field "handle".
func (h *Handler) ServeLogin(w http.ResponseWriter, r *http.Request) { h.serveLogin(w, r, h.cfg.Auth) }

// ServeConnect starts the sign-in for the filtered feeds, which also signs the browser in. It
// answers 404 when they are off.
func (h *Handler) ServeConnect(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Connect == nil {
		http.NotFound(w, r)
		return
	}
	h.serveLogin(w, r, h.cfg.Connect)
}

// ServeConnectCallback is where a viewer's server sends them back from the sign-in for the
// filtered feeds.
func (h *Handler) ServeConnectCallback(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Connect == nil {
		http.NotFound(w, r)
		return
	}
	h.serveCallback(w, r, h.cfg.Connect)
}

// ServeConnectMetadata serves the client metadata of the sign-in for the filtered feeds.
func (h *Handler) ServeConnectMetadata(w http.ResponseWriter, r *http.Request) {
	if h.cfg.Connect == nil {
		http.NotFound(w, r)
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(h.cfg.Connect.Metadata())
}

// ServeDisconnect revokes and forgets the signed-in viewer's sign-in for the filtered feeds. The
// browser stays signed in.
func (h *Handler) ServeDisconnect(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if h.cfg.Connect == nil {
		http.NotFound(w, r)
		return
	}
	if !SameOrigin(r, h.cfg.Origin) {
		http.Error(w, "cross-site request", http.StatusForbidden)
		return
	}
	did, ok := h.Viewer(r)
	if !ok {
		replyJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), loginTimeout)
	defer cancel()
	if err := h.cfg.Connect.Disconnect(ctx, did); err != nil {
		h.cfg.Log.Warn("filtered feeds: signing out failed", "did", did, "err", err)
		replyJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
		return
	}
	replyJSON(w, http.StatusOK, map[string]bool{"connected": false})
}

func (h *Handler) serveLogin(w http.ResponseWriter, r *http.Request, auth Authenticator) {
	noStore(w)
	if !SameOrigin(r, h.cfg.Origin) {
		http.Error(w, "cross-site request", http.StatusForbidden)
		return
	}
	if h.cfg.Allow != nil && !h.cfg.Allow(r) {
		if wantsJSON(r) {
			replyJSON(w, http.StatusTooManyRequests, map[string]string{"error": "limited"})
			return
		}
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return
	}
	r.Body = http.MaxBytesReader(w, r.Body, maxFormBytes)
	if err := r.ParseForm(); err != nil {
		h.problem(w, r, "invalid", http.StatusBadRequest)
		return
	}
	account := normalizeAccount(r.PostForm.Get("handle"))
	if account == "" || len(account) > maxAccountLen {
		h.problem(w, r, "invalid", http.StatusBadRequest)
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), loginTimeout)
	defer cancel()
	redirect, state, err := auth.Start(ctx, account)
	switch {
	case errors.Is(err, ErrBadAccount):
		h.problem(w, r, "invalid", http.StatusBadRequest)
		return
	case errors.Is(err, ErrBusy):
		h.cfg.Log.Warn("sign-in: too many logins in progress")
		h.problem(w, r, "busy", http.StatusServiceUnavailable)
		return
	case err != nil:
		h.cfg.Log.Info("sign-in: starting a login failed", "account", account, "err", err)
		h.problem(w, r, "failed", http.StatusBadGateway)
		return
	}
	if u, err := url.Parse(redirect); err != nil || (u.Scheme != "https" && u.Scheme != "http") || u.Host == "" {
		h.cfg.Log.Warn("sign-in: the authorization server gave a redirect that is not a web address", "account", account)
		h.problem(w, r, "failed", http.StatusBadGateway)
		return
	}
	// The login is tied to this browser, so a link that finishes someone else's login can't
	// sign this browser in as them.
	http.SetCookie(w, h.stateCookie(state, int(pendingTTL.Seconds())))
	// Where to come back to. A path that isn't allowed clears any left from an earlier login.
	if back := h.allowedReturn(r.PostForm.Get("return")); back != "" {
		http.SetCookie(w, h.returnCookie(back, int(pendingTTL.Seconds())))
	} else {
		http.SetCookie(w, h.returnCookie("", -1))
	}
	if wantsJSON(r) {
		replyJSON(w, http.StatusOK, map[string]string{"redirect": redirect})
		return
	}
	http.Redirect(w, r, redirect, http.StatusSeeOther)
}

// ServeCallback is where the person's own server sends them back.
func (h *Handler) ServeCallback(w http.ResponseWriter, r *http.Request) {
	h.serveCallback(w, r, h.cfg.Auth)
}

func (h *Handler) serveCallback(w http.ResponseWriter, r *http.Request, auth Authenticator) {
	noStore(w)
	q := r.URL.Query()
	state := q.Get("state")
	// The login is over either way: this browser is done with its state cookie.
	bound, err := r.Cookie(h.stateKey)
	http.SetCookie(w, h.stateCookie("", -1))
	http.SetCookie(w, h.returnCookie("", -1)) // read from the request by home, below
	if err != nil || state == "" || subtle.ConstantTimeCompare([]byte(bound.Value), []byte(state)) != 1 {
		// Say which: a browser that never sent the cookie (it was refused, or blocked) is a very
		// different problem from one that sent a cookie for another login.
		reason := "the browser sent no login cookie"
		if err == nil {
			reason = "the login cookie is for another login"
		}
		if state == "" {
			reason = "the callback has no state"
		}
		h.cfg.Log.Info("sign-in: a callback that this browser didn't start", "reason", reason)
		h.home(w, r, "failed")
		return
	}
	if q.Get("error") != "" {
		h.home(w, r, "denied") // they said no, or their server did
		return
	}
	code := q.Get("code")
	if code == "" {
		h.home(w, r, "failed")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), loginTimeout)
	defer cancel()
	did, err := auth.Finish(ctx, code, state, q.Get("iss"))
	if err != nil {
		h.cfg.Log.Info("sign-in: finishing a login failed", "err", err)
		h.home(w, r, "failed")
		return
	}
	http.SetCookie(w, &http.Cookie{Name: h.sessionKey, Value: h.cfg.Sessions.Issue(did), Path: "/",
		MaxAge: int(h.cfg.Sessions.TTL().Seconds()), HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteLaxMode})
	h.cfg.Log.Info("sign-in: signed in", "did", did)
	h.home(w, r, "")
}

// ServeLogout signs this browser out.
func (h *Handler) ServeLogout(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if !SameOrigin(r, h.cfg.Origin) {
		http.Error(w, "cross-site request", http.StatusForbidden)
		return
	}
	http.SetCookie(w, &http.Cookie{Name: h.sessionKey, Value: "", Path: "/", MaxAge: -1,
		HttpOnly: true, Secure: h.secure, SameSite: http.SameSiteLaxMode})
	if wantsJSON(r) {
		w.WriteHeader(http.StatusNoContent)
		return
	}
	h.home(w, r, "")
}

// ServeMe says who is signed in: {"did", "handle"}, plus "owner": true for the service's owner, or 401.
func (h *Handler) ServeMe(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	w.Header().Set("Content-Type", "application/json")
	did, ok := h.Viewer(r)
	if !ok {
		w.WriteHeader(http.StatusUnauthorized)
		_, _ = w.Write([]byte(`{"error":"not signed in"}` + "\n"))
		return
	}
	handle := ""
	if h.cfg.Handle != nil {
		ctx, cancel := context.WithTimeout(r.Context(), 3*time.Second)
		defer cancel()
		handle = h.cfg.Handle(ctx, did)
	}
	_ = json.NewEncoder(w).Encode(meResponse{DID: did, Handle: handle, Owner: h.cfg.Owner != "" && did == h.cfg.Owner})
}

// meResponse is what /api/me answers. Owner is left out unless it is true.
type meResponse struct {
	DID    string `json:"did"`
	Handle string `json:"handle"`
	Owner  bool   `json:"owner,omitempty"`
}
