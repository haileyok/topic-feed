package feedgen

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"mime"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/haileyok/topic-feed/internal/signin"
)

// maxFiltersBody bounds a viewer's filters as sent: every topic and score set is a few kilobytes.
const maxFiltersBody = 64 << 10

// FilteredAPI is the page at /filtered's API (/api/me/filtered): whether the signed-in viewer has
// signed in for the filtered feeds, and their filters for each. The viewer is whoever the signed
// cookie says, never anything in the request.
type FilteredAPI struct {
	Viewer func(r *http.Request) (did string, ok bool)
	// Connected says whether a viewer has a sign-in for the filtered feeds (signin.Connector).
	Connected func(ctx context.Context, did string) (bool, error)
	Feeds     *Feeds
	Sources   *Sources
	Store     FilteredStore
	Service   *FilteredFeeds // told when a viewer's filters change
	OwnerDID  string
	Paths     map[string]bool
	Origin    string
	Edits     *IPLimiter
	Log       *slog.Logger
}

// FilteredFeedView is a filtered feed as the page shows it.
type FilteredFeedView struct {
	Rkey        string `json:"rkey"`
	Name        string `json:"name"`
	Description string `json:"description"`
	URL         string `json:"url"` // the feed in the Bluesky app
	Source      string `json:"source"`
	SourceName  string `json:"sourceName,omitempty"`
	SourceURL   string `json:"sourceUrl,omitempty"`
	// Defaults are the feed's own filters; Filters the viewer's, or null when they chose none.
	Defaults Filters  `json:"defaults"`
	Filters  *Filters `json:"filters"`
}

func (a *FilteredAPI) feeds() []Feed {
	var out []Feed
	for _, f := range a.Feeds.List() {
		if f.Filtered != nil {
			out = append(out, f)
		}
	}
	return out
}

// sourceURL is the at:// URI of a feed as a link to it in the Bluesky app.
func sourceURL(uri string) string {
	rest, ok := strings.CutPrefix(uri, "at://")
	if !ok {
		return ""
	}
	parts := strings.Split(rest, "/")
	if len(parts) != 3 {
		return ""
	}
	return "https://bsky.app/profile/" + parts[0] + "/feed/" + parts[2]
}

// ServeList answers GET /api/me/filtered.
func (a *FilteredAPI) ServeList(w http.ResponseWriter, r *http.Request) {
	meHeaders(w)
	did, ok := a.Viewer(r)
	if !ok {
		meJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		return
	}
	if a.Edits != nil && !a.Edits.Allow(did) {
		w.Header().Set("Retry-After", "2")
		meJSON(w, http.StatusTooManyRequests, map[string]string{"error": "limited"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	connected, err := a.Connected(ctx, did)
	if err != nil {
		a.Log.Error("filtered feeds: reading a sign-in failed", "did", did, "err", err)
		failure(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	views := []FilteredFeedView{}
	for _, f := range a.feeds() {
		own, err := a.Store.ViewerFilters(ctx, did, f.Rkey)
		if err != nil {
			a.Log.Error("filtered feeds: reading a viewer's filters failed", "did", did, "feed", f.Rkey, "err", err)
			failure(w, http.StatusServiceUnavailable, "unavailable", "")
			return
		}
		v := FilteredFeedView{Rkey: f.Rkey, Name: f.DisplayName, Description: f.Description,
			URL:    "https://bsky.app/profile/" + a.OwnerDID + "/feed/" + f.Rkey,
			Source: f.Filtered.Source, SourceURL: sourceURL(f.Filtered.Source), Defaults: f.Filters(), Filters: own}
		if src, ok := a.Sources.Get(f.Filtered.Source); ok {
			v.SourceName = src.DisplayName
		}
		views = append(views, v)
	}
	meJSON(w, http.StatusOK, map[string]any{"did": did, "connected": connected, "feeds": views,
		"limits": map[string]int{"maxExclude": maxFilterExclude, "maxTopicRules": maxTopicRules}})
}

// ServeSave answers PUT /api/me/filtered/{rkey}: the body is the viewer's filters for that feed,
// whole, or null to go back to the feed's own.
func (a *FilteredAPI) ServeSave(w http.ResponseWriter, r *http.Request) {
	meHeaders(w)
	did, ok := a.Viewer(r)
	if !ok {
		meJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		return
	}
	if !signin.SameOrigin(r, a.Origin) {
		failure(w, http.StatusForbidden, "forbidden", "")
		return
	}
	if mt, _, err := mime.ParseMediaType(r.Header.Get("Content-Type")); err != nil || mt != "application/json" {
		failure(w, http.StatusUnsupportedMediaType, "unsupported", "send JSON")
		return
	}
	if a.Edits != nil && !a.Edits.Allow(did) {
		w.Header().Set("Retry-After", strconv.Itoa(2))
		meJSON(w, http.StatusTooManyRequests, map[string]string{"error": "limited"})
		return
	}
	rkey := r.PathValue("rkey")
	var feed *Feed
	for _, f := range a.feeds() {
		if f.Rkey == rkey {
			feed = &f
		}
	}
	if feed == nil {
		failure(w, http.StatusNotFound, "not_found", "There's no filtered feed with that key.")
		return
	}
	fl, err := decodeFilters(http.MaxBytesReader(w, r.Body, maxFiltersBody))
	if err != nil {
		var tooBig *http.MaxBytesError
		if errors.As(err, &tooBig) {
			failure(w, http.StatusRequestEntityTooLarge, "too_large", "Those filters are too big.")
			return
		}
		failure(w, http.StatusBadRequest, "invalid", "Those aren't filters: "+err.Error())
		return
	}
	if fl != nil {
		if err := fl.validate(a.Paths); err != nil {
			failure(w, http.StatusBadRequest, "invalid", err.Error())
			return
		}
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	if err := a.Store.SaveViewerFilters(ctx, did, rkey, fl); err != nil {
		a.Log.Error("filtered feeds: saving a viewer's filters failed", "did", did, "feed", rkey, "err", err)
		failure(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	if a.Service != nil {
		a.Service.Forget(did, rkey)
	}
	meJSON(w, http.StatusOK, map[string]any{"rkey": rkey, "filters": fl})
}

// leftOutPage is how many left-out posts one request may ask for.
const leftOutPage = 200

// ServeLeftOut answers GET /api/me/filtered/{rkey}/left-out: the posts that feed left out of the
// signed-in viewer's own feed in the last week, each once, newest first, a page at a time (?limit=,
// at most 200, default 50; ?cursor= from the last page's "cursor", which is "" at the end).
func (a *FilteredAPI) ServeLeftOut(w http.ResponseWriter, r *http.Request) {
	meHeaders(w)
	did, ok := a.Viewer(r)
	if !ok {
		meJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		return
	}
	if a.Edits != nil && !a.Edits.Allow(did) {
		w.Header().Set("Retry-After", "2")
		meJSON(w, http.StatusTooManyRequests, map[string]string{"error": "limited"})
		return
	}
	rkey := r.PathValue("rkey")
	if !slices.ContainsFunc(a.feeds(), func(f Feed) bool { return f.Rkey == rkey }) {
		failure(w, http.StatusNotFound, "not_found", "There's no filtered feed with that key.")
		return
	}
	limit := 50
	var after LeftOutCursor
	if v := r.URL.Query().Get("cursor"); v != "" {
		at, uri, ok := strings.Cut(v, "|")
		t, err := time.Parse(time.RFC3339Nano, at)
		if !ok || err != nil || uri == "" {
			failure(w, http.StatusBadRequest, "invalid", "that isn't a cursor from this page")
			return
		}
		after = LeftOutCursor{At: t, URI: uri}
	}
	if v := r.URL.Query().Get("limit"); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n < 1 || n > leftOutPage {
			failure(w, http.StatusBadRequest, "invalid", "limit must be 1-200")
			return
		}
		limit = n
	}
	ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
	defer cancel()
	posts, err := a.Store.LeftOutPosts(ctx, did, rkey, after, limit)
	if err != nil {
		a.Log.Error("filtered feeds: reading left-out posts failed", "did", did, "feed", rkey, "err", err)
		failure(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	if posts == nil {
		posts = []LeftOutPost{}
	}
	cursor := ""
	if len(posts) == limit {
		last := posts[len(posts)-1]
		cursor = last.At.UTC().Format(time.RFC3339Nano) + "|" + last.URI
	}
	name := ""
	for _, f := range a.feeds() {
		if f.Rkey == rkey {
			name = f.DisplayName
		}
	}
	meJSON(w, http.StatusOK, map[string]any{"rkey": rkey, "name": name, "posts": posts, "cursor": cursor})
}

// decodeFilters reads one set of filters, or null; it refuses fields it doesn't know and anything
// after the filters but space.
func decodeFilters(r io.Reader) (*Filters, error) {
	dec := json.NewDecoder(r)
	dec.DisallowUnknownFields()
	var fl *Filters
	if err := dec.Decode(&fl); err != nil {
		return nil, err
	}
	if _, err := dec.Token(); !errors.Is(err, io.EOF) {
		if err == nil {
			err = errors.New("unexpected data after the filters")
		}
		return nil, err
	}
	return fl, nil
}
