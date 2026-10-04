package feedgen

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"mime"
	"net/http"
	"strconv"
	"sync"
	"time"

	"github.com/haileyok/topic-feed/internal/signin"
)

const (
	// maxFeedBody bounds what is read of a feed somebody saves: a feed with every topic and every
	// score set is a few kilobytes.
	maxFeedBody  = 64 << 10
	feedsTimeout = 10 * time.Second
)

// BrowserScope is what the page that publishes feeds asks Bluesky for, in the browser: to write
// feed records (and only those) into the account's own repo. It is granular permissions only, never
// mixed with the broad transitional ones; the service never holds the tokens. A feed's picture is not
// uploaded from here, so there is no permission to upload (a picture set in the Bluesky app is kept
// when the page updates the feed).
const BrowserScope = "atproto repo:app.bsky.feed.generator"

// FeedsAPI is what a signed-in person can do with their own feeds (/api/me/feeds): list, save and
// delete them. The account is whatever the signed cookie says and nothing in the request: no
// parameter, header or path chooses whose feeds these are.
type FeedsAPI struct {
	// Viewer says who is signed in on a request, if anyone (signin.Handler.Viewer).
	Viewer func(r *http.Request) (did string, ok bool)
	// Owner is the DID of the service owner, who has no limits on their feeds and may make feeds
	// that take adult posts.
	Owner string
	// ServiceDID is the DID the feed records point at (their `did` field).
	ServiceDID string
	Store      FeedStore
	// Feeds is told to look at the feeds again after a change; nil: nothing is told.
	Feeds *Feeds
	// Paths are the topics a feed may name (TaxonomyPaths).
	Paths  map[string]bool
	Limits UserLimits
	// Origin is this site's origin (https://host): changes must come from it.
	Origin string
	// Edits limits how often one account can read or change their feeds (keyed by DID).
	Edits *IPLimiter
	Log   *slog.Logger

	mu sync.Mutex // one change at a time: the cap on feeds is checked and then used
}

// FeedView is a feed as the page shows it.
type FeedView struct {
	Rkey      string    `json:"rkey"`
	URI       string    `json:"uri"` // of its record in the account's repo
	Spec      FeedSpec  `json:"spec"`
	CreatedAt time.Time `json:"createdAt"`
	UpdatedAt time.Time `json:"updatedAt"`
}

// FeedLimits are what the page tells a person they may do; 0 is no limit.
type FeedLimits struct {
	MaxFeeds       int `json:"maxFeeds"`
	MaxPaths       int `json:"maxPaths"`
	MaxExclude     int `json:"maxExclude"`
	MaxPosts       int `json:"maxPosts"`
	MaxName        int `json:"maxName"`
	MaxDescription int `json:"maxDescription"`
}

// FeedsResponse is the answer to GET /api/me/feeds.
type FeedsResponse struct {
	DID        string     `json:"did"`
	Owner      bool       `json:"owner"`
	ServiceDID string     `json:"serviceDid"`
	Scope      string     `json:"scope"`
	Feeds      []FeedView `json:"feeds"`
	Limits     FeedLimits `json:"limits"`
}

func (a *FeedsAPI) signedIn(w http.ResponseWriter, r *http.Request) (string, bool) {
	meHeaders(w)
	did, ok := a.Viewer(r)
	if !ok {
		meJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		return "", false
	}
	return did, true
}

func (a *FeedsAPI) limited(w http.ResponseWriter, did string) bool {
	if a.Edits == nil || a.Edits.Allow(did) {
		return false
	}
	w.Header().Set("Retry-After", strconv.Itoa(2))
	meJSON(w, http.StatusTooManyRequests, map[string]string{"error": "limited"})
	return true
}

func (a *FeedsAPI) isOwner(did string) bool { return did == a.Owner }

func (a *FeedsAPI) view(s StoredFeed) FeedView {
	return FeedView{Rkey: s.Rkey, URI: "at://" + s.Owner + "/" + generatorCollection + "/" + s.Rkey, Spec: s.Spec,
		CreatedAt: s.CreatedAt, UpdatedAt: s.UpdatedAt}
}

func (a *FeedsAPI) limitsFor(did string) FeedLimits {
	l := FeedLimits{MaxName: maxDisplayName, MaxDescription: maxDescription}
	if !a.isOwner(did) {
		l.MaxFeeds, l.MaxPaths, l.MaxExclude, l.MaxPosts = a.Limits.MaxFeeds, a.Limits.MaxPaths, a.Limits.MaxExclude, a.Limits.MaxPosts
	}
	return l
}

func (a *FeedsAPI) changed() {
	if a.Feeds != nil {
		a.Feeds.SyncNow()
	}
}

// ServeList answers GET /api/me/feeds: the signed-in account's feeds, and what they may do with them.
func (a *FeedsAPI) ServeList(w http.ResponseWriter, r *http.Request) {
	did, ok := a.signedIn(w, r)
	if !ok || a.limited(w, did) {
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), feedsTimeout)
	defer cancel()
	stored, err := a.Store.OwnerFeeds(ctx, did)
	if err != nil {
		a.Log.Error("feeds: reading an account's feeds failed", "account", did, "err", err)
		failure(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	resp := FeedsResponse{DID: did, Owner: a.isOwner(did), ServiceDID: a.ServiceDID, Scope: BrowserScope,
		Feeds: make([]FeedView, 0, len(stored)), Limits: a.limitsFor(did)}
	for _, s := range stored {
		resp.Feeds = append(resp.Feeds, a.view(s))
	}
	meJSON(w, http.StatusOK, resp)
}

// readChange checks a request that changes feeds. It must come from this site's own pages, and
// when it carries a feed that must be JSON (a browser won't send that cross-site without asking
// first). It answers the request itself and returns false if anything is wrong.
func (a *FeedsAPI) readChange(w http.ResponseWriter, r *http.Request, wantBody bool) bool {
	if !signin.SameOrigin(r, a.Origin) {
		failure(w, http.StatusForbidden, "forbidden", "")
		return false
	}
	if wantBody {
		if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
			failure(w, http.StatusUnsupportedMediaType, "unsupported", "send JSON")
			return false
		}
	}
	return true
}

// ServeSave answers PUT /api/me/feeds/{rkey}: the body is the feed, whole. It makes the feed or
// replaces the one the account has with that rkey. The feed is the account's whichever rkey it names.
func (a *FeedsAPI) ServeSave(w http.ResponseWriter, r *http.Request) {
	did, ok := a.signedIn(w, r)
	if !ok || !a.readChange(w, r, true) || a.limited(w, did) {
		return
	}
	rkey := r.PathValue("rkey")
	if !rkeyPattern.MatchString(rkey) {
		failure(w, http.StatusBadRequest, "invalid", "The feed's key must be 1-15 lowercase letters, digits or dashes, starting with a letter or digit.")
		return
	}
	spec, err := DecodeFeedSpec(http.MaxBytesReader(w, r.Body, maxFeedBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			failure(w, http.StatusRequestEntityTooLarge, "too_large", "That feed is too big.")
			return
		}
		failure(w, http.StatusBadRequest, "invalid", "That isn't a feed: "+err.Error())
		return
	}
	f := spec.Feed("", rkey)
	if err := f.validate(a.Paths); err != nil {
		failure(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	if !a.isOwner(did) {
		if err := f.validateUser(a.Limits); err != nil {
			failure(w, http.StatusBadRequest, "invalid", err.Error())
			return
		}
	}

	ctx, cancel := context.WithTimeout(r.Context(), feedsTimeout)
	defer cancel()
	a.mu.Lock()
	defer a.mu.Unlock()
	existing, err := a.Store.OwnerFeeds(ctx, did)
	if err != nil {
		a.Log.Error("feeds: reading an account's feeds failed", "account", did, "err", err)
		failure(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	var created time.Time
	exists := false
	for _, s := range existing {
		if s.Rkey == rkey {
			exists, created = true, s.CreatedAt
		}
	}
	if !exists && !a.isOwner(did) && len(existing) >= a.Limits.MaxFeeds {
		failure(w, http.StatusConflict, "limit", "You can have at most "+strconv.Itoa(a.Limits.MaxFeeds)+" feeds. Delete one to make another.")
		return
	}
	saved := StoredFeed{Owner: did, Rkey: rkey, Spec: spec, CreatedAt: created}
	if err := a.Store.SaveFeed(ctx, saved); err != nil {
		a.Log.Error("feeds: saving a feed failed", "account", did, "feed", rkey, "err", err)
		failure(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	a.changed()
	// Read it back: what is shown is what is stored, with the dates the store gave it.
	if back, err := a.Store.OwnerFeeds(ctx, did); err == nil {
		for _, s := range back {
			if s.Rkey == rkey {
				saved = s
			}
		}
	}
	status := http.StatusOK
	if !exists {
		status = http.StatusCreated
	}
	meJSON(w, status, map[string]any{"feed": a.view(saved), "created": !exists})
}

// ServeDelete answers DELETE /api/me/feeds/{rkey}: the account's feed with that rkey is removed.
// Removing one that isn't there is not an error. (Taking its record out of the account's repo is
// done in the browser, which holds the permission to.)
func (a *FeedsAPI) ServeDelete(w http.ResponseWriter, r *http.Request) {
	did, ok := a.signedIn(w, r)
	if !ok || !a.readChange(w, r, false) || a.limited(w, did) {
		return
	}
	rkey := r.PathValue("rkey")
	if !rkeyPattern.MatchString(rkey) {
		failure(w, http.StatusBadRequest, "invalid", "That isn't the key of a feed.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), feedsTimeout)
	defer cancel()
	a.mu.Lock()
	defer a.mu.Unlock()
	if err := a.Store.DeleteFeed(ctx, did, rkey); err != nil {
		a.Log.Error("feeds: deleting a feed failed", "account", did, "feed", rkey, "err", err)
		failure(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	a.changed()
	meJSON(w, http.StatusOK, map[string]any{"deleted": rkey})
}

// ServeClientMetadata answers GET /oauth/browser-client-metadata.json: what Bluesky's authorization
// servers read to know the page that publishes feeds. This is a client of its own, apart from the
// one that signs people in: it runs in the browser, which keeps the tokens, and asks only for the
// permission to write feed records.
func (a *FeedsAPI) ServeClientMetadata(w http.ResponseWriter, _ *http.Request) {
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "public, max-age=300")
	w.Header().Set("X-Content-Type-Options", "nosniff")
	_ = json.NewEncoder(w).Encode(BrowserClientMetadata(a.Origin))
}

// BrowserClientMetadata is the OAuth client metadata of the page that publishes feeds, for the site
// at origin (https://host). Its client ID is the address it is served from.
func BrowserClientMetadata(origin string) map[string]any {
	return map[string]any{
		"client_id":                  origin + "/oauth/browser-client-metadata.json",
		"client_name":                "Feeds at " + hostOf(origin),
		"client_uri":                 origin,
		"redirect_uris":              []string{origin + "/feeds"},
		"scope":                      BrowserScope,
		"grant_types":                []string{"authorization_code", "refresh_token"},
		"response_types":             []string{"code"},
		"token_endpoint_auth_method": "none",
		"application_type":           "web",
		"dpop_bound_access_tokens":   true,
	}
}

func hostOf(origin string) string {
	const scheme = "https://"
	if len(origin) > len(scheme) && origin[:len(scheme)] == scheme {
		return origin[len(scheme):]
	}
	return origin
}
