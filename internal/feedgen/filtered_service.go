package feedgen

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/haileyok/topic-feed/internal/signin"
)

// TokenSource asks a viewer's own server for a token for a source feed: signin.Connector. It
// returns signin.ErrNotConnected when the viewer has no sign-in that works.
type TokenSource interface {
	Token(ctx context.Context, did, aud, lxm string) (string, error)
}

const (
	// filteredPages is the most source pages one request reads, and filteredBudget how long it may
	// spend: a source whose posts the filters nearly all leave out still has to be answered.
	filteredPages  = 10
	filteredBudget = 6 * time.Second
	// minSourcePage is the fewest posts asked of the source at a time.
	minSourcePage = 30
	// stashFor is how long the posts held over for a viewer's next page are kept.
	stashFor      = 30 * time.Minute
	maxStashes    = 100_000
	filtersFor    = 30 * time.Second // how long a viewer's filters are reused before being read again
	maxSourceBody = 4 << 20
)

// errSourceFailed means the source feed didn't answer as a feed: it is down, or refused the token.
var errSourceFailed = errors.New("the source feed failed")

// FilteredFeeds serves the filtered feeds: each request reads the source feed with a token the
// viewer's own server signed, leaves out the posts the viewer's filters (or the feed's) don't
// keep, and reads on until it has as many as were asked for.
type FilteredFeeds struct {
	Sources *Sources
	Store   FilteredStore
	Tokens  TokenSource
	HTTP    *http.Client
	// LeftOut records the posts left out of each viewer's feed, for them to look back at; nil: not kept.
	LeftOut LeftOutSink
	// SignInPost is the at:// URI of the post shown, alone, to viewers who aren't signed in for the
	// filtered feeds; "": they get an empty feed.
	SignInPost string
	Log        *slog.Logger
	Now        func() time.Time

	// forwards bounds the interactions being sent on at once; nil: no bound.
	forwards chan struct{}

	mu      sync.Mutex
	stashes map[string]stash
	filters map[filtersKey]cachedFilters
}

type stash struct {
	viewer, feed string
	items        []skeletonItem
	expires      time.Time
}

type filtersKey struct{ did, rkey string }

type cachedFilters struct {
	filters Filters
	at      time.Time
}

// FilteredPage is a page of a filtered feed. State says what it is: "ok", or "signin" (the sign-in
// post, alone).
type FilteredPage struct {
	Items  []skeletonItem
	Cursor string
	State  string
}

// NewFilteredFeeds makes the service.
func NewFilteredFeeds(sources *Sources, store FilteredStore, tokens TokenSource, log *slog.Logger) *FilteredFeeds {
	return &FilteredFeeds{Sources: sources, Store: store, Tokens: tokens, Log: log,
		HTTP: &http.Client{Timeout: 5 * time.Second}, forwards: make(chan struct{}, 64)}
}

func (ff *FilteredFeeds) now() time.Time {
	if ff.Now != nil {
		return ff.Now()
	}
	return time.Now()
}

// filteredCursor is the cursor of a filtered feed's page: the source's cursor to read on from, and
// the key of the posts held over (read from the source already, and kept, but beyond the page).
type filteredCursor struct {
	Source string `json:"c,omitempty"`
	Stash  string `json:"s,omitempty"`
	End    bool   `json:"e,omitempty"` // the source has no more: only the held posts are left
}

func (c filteredCursor) encode() string {
	b, _ := json.Marshal(c)
	return base64.RawURLEncoding.EncodeToString(b)
}

func decodeFilteredCursor(s string) (filteredCursor, error) {
	var c filteredCursor
	if s == "" {
		return c, nil
	}
	b, err := base64.RawURLEncoding.DecodeString(s)
	if err != nil || len(b) > 4096 || json.Unmarshal(b, &c) != nil {
		return c, errBadCursor
	}
	return c, nil
}

// Sources' request IDs travel inside each post's feed context, so the interactions that come back
// with it can be sent on with the request ID the source gave that post. wrapContext and
// unwrapContext are each other's inverse.
const contextPrefix = "fr1:"

func wrapContext(reqID, feedContext string) string {
	return contextPrefix + base64.RawURLEncoding.EncodeToString([]byte(reqID)) + ":" + feedContext
}

func unwrapContext(s string) (reqID, feedContext string) {
	rest, ok := strings.CutPrefix(s, contextPrefix)
	if !ok {
		return "", s
	}
	enc, ctx, ok := strings.Cut(rest, ":")
	if !ok {
		return "", s
	}
	id, err := base64.RawURLEncoding.DecodeString(enc)
	if err != nil {
		return "", s
	}
	return string(id), ctx
}

// signInPage is what a viewer without a sign-in that works gets: the sign-in post and nothing else.
func (ff *FilteredFeeds) signInPage() FilteredPage {
	p := FilteredPage{Items: []skeletonItem{}, State: "signin"}
	if ff.SignInPost != "" {
		p.Items = append(p.Items, skeletonItem{Post: ff.SignInPost})
	}
	return p
}

// Page is up to limit posts of the filtered feed f for viewer ("" when the request had no valid
// credential), after cursor. Headers are the viewer's request's, some of which are passed on.
func (ff *FilteredFeeds) Page(ctx context.Context, f Feed, viewer, cursor string, limit int, headers http.Header) (FilteredPage, error) {
	if viewer == "" {
		return ff.signInPage(), nil
	}
	cur, err := decodeFilteredCursor(cursor)
	if err != nil {
		return FilteredPage{}, err
	}
	src, ok := ff.Sources.Get(f.Filtered.Source)
	if !ok {
		return FilteredPage{}, errNotReady
	}
	token, err := ff.Tokens.Token(ctx, viewer, src.Audience(), signin.FeedSkeletonMethod)
	if errors.Is(err, signin.ErrNotConnected) {
		return ff.signInPage(), nil
	}
	if err != nil {
		return FilteredPage{}, fmt.Errorf("a token for %s: %w", src.ServiceDID, err)
	}
	filters, err := ff.viewerFilters(ctx, viewer, f)
	if err != nil {
		return FilteredPage{}, err
	}

	kept := ff.takeStash(cur.Stash, viewer, f.Key())
	seen := map[string]bool{}
	for _, it := range kept {
		seen[it.Post] = true
	}
	srcCursor, end := cur.Source, cur.End
	deadline := ff.now().Add(filteredBudget)
	for pages := 0; len(kept) < limit && !end && pages < filteredPages && ff.now().Before(deadline); pages++ {
		page, err := ff.fetch(ctx, f.Rkey, src, token, srcCursor, max(limit, minSourcePage), headers)
		if err != nil {
			if pages == 0 && len(kept) == 0 {
				return FilteredPage{}, err
			}
			ff.Log.Warn("filtered feeds: a later page of the source failed; answering with what was read", "source", src.URI, "err", err)
			break
		}
		uris := make([]string, 0, len(page.Feed))
		for _, it := range page.Feed {
			uris = append(uris, it.Post)
		}
		scores, err := ff.Store.ScorePosts(ctx, uris)
		if err != nil {
			return FilteredPage{}, err
		}
		dropped := 0
		var leftOut []LeftOutRow
		for _, it := range page.Feed {
			if it.Post == "" || seen[it.Post] {
				continue
			}
			p, ok := scores[it.Post]
			if !ok {
				p = ScoredPost{URI: it.Post}
			}
			if why := filters.Why(p); why != nil {
				dropped++
				leftOut = append(leftOut, LeftOutRow{ViewerDID: viewer, Feed: f.Rkey, URI: it.Post, Reason: why.Kind, Name: why.Name,
					Value: why.Value, Cutoff: why.Cutoff, Bound: why.Bound, Rule: why.Rule, LeftOutAt: ff.now().UTC()})
				continue
			}
			seen[it.Post] = true
			kept = append(kept, skeletonItem{Post: it.Post, Reason: it.Reason, FeedContext: wrapContext(page.ReqID, it.FeedContext)})
		}
		if ff.LeftOut != nil && len(leftOut) > 0 {
			ff.LeftOut.Add(leftOut)
		}
		metricFilteredPosts.WithLabelValues(f.Rkey, "read").Add(float64(len(page.Feed)))
		metricFilteredPosts.WithLabelValues(f.Rkey, "dropped").Add(float64(dropped))
		// A source that answers with the cursor it was given, or no posts, has no more.
		if page.Cursor == "" || page.Cursor == srcCursor || len(page.Feed) == 0 {
			end = true
		}
		srcCursor = page.Cursor
	}

	out := FilteredPage{Items: kept, State: "ok"}
	if len(kept) > limit {
		out.Items = kept[:limit]
		next := filteredCursor{Source: srcCursor, End: end, Stash: ff.putStash(viewer, f.Key(), kept[limit:])}
		out.Cursor = next.encode()
	} else if !end {
		out.Cursor = filteredCursor{Source: srcCursor}.encode()
	}
	if out.Items == nil {
		out.Items = []skeletonItem{}
	}
	return out, nil
}

// viewerFilters is the filters the viewer chose for the feed, or the feed's own.
func (ff *FilteredFeeds) viewerFilters(ctx context.Context, did string, f Feed) (Filters, error) {
	k := filtersKey{did, f.Rkey}
	now := ff.now()
	ff.mu.Lock()
	if c, ok := ff.filters[k]; ok && now.Sub(c.at) < filtersFor {
		ff.mu.Unlock()
		return c.filters, nil
	}
	ff.mu.Unlock()
	own, err := ff.Store.ViewerFilters(ctx, did, f.Rkey)
	if err != nil {
		return Filters{}, err
	}
	fl := f.Filters()
	if own != nil {
		fl = *own
	}
	ff.mu.Lock()
	if ff.filters == nil || len(ff.filters) > maxStashes {
		ff.filters = map[filtersKey]cachedFilters{}
	}
	ff.filters[k] = cachedFilters{filters: fl, at: now}
	ff.mu.Unlock()
	return fl, nil
}

// Forget drops what is remembered of a viewer's filters for the feed rkey (after they saved new ones).
func (ff *FilteredFeeds) Forget(did, rkey string) {
	ff.mu.Lock()
	delete(ff.filters, filtersKey{did, rkey})
	ff.mu.Unlock()
}

func (ff *FilteredFeeds) putStash(viewer, feed string, items []skeletonItem) string {
	b := make([]byte, 12)
	rand.Read(b)
	id := hex.EncodeToString(b)
	now := ff.now()
	ff.mu.Lock()
	defer ff.mu.Unlock()
	if ff.stashes == nil {
		ff.stashes = map[string]stash{}
	}
	if len(ff.stashes) >= maxStashes {
		for k, s := range ff.stashes {
			if !now.Before(s.expires) {
				delete(ff.stashes, k)
			}
		}
		for k := range ff.stashes { // still full: drop any
			if len(ff.stashes) < maxStashes {
				break
			}
			delete(ff.stashes, k)
		}
	}
	ff.stashes[id] = stash{viewer: viewer, feed: feed, items: items, expires: now.Add(stashFor)}
	return id
}

// takeStash is the posts held over under id for this viewer and feed, if they are still kept. They
// stay kept: the same cursor asked for again (a retry) gets the same posts.
func (ff *FilteredFeeds) takeStash(id, viewer, feed string) []skeletonItem {
	if id == "" {
		return nil
	}
	ff.mu.Lock()
	defer ff.mu.Unlock()
	s, ok := ff.stashes[id]
	if !ok || s.viewer != viewer || s.feed != feed || !ff.now().Before(s.expires) {
		return nil
	}
	return append([]skeletonItem(nil), s.items...)
}

// Interaction is one interaction as sendInteractions carries it.
type Interaction struct {
	Item        string `json:"item"`
	Event       string `json:"event"`
	FeedContext string `json:"feedContext,omitempty"`
	ReqID       string `json:"reqId,omitempty"`
}

// discoverService is Bluesky's Discover feed, which the Bluesky app sends interactions to though
// its record doesn't ask for them.
const discoverService = "did:web:discover.bsky.app"

// wantsInteractions reports whether interactions are sent on to the source: when its record asks
// for them, or it is Discover.
func (s FeedSource) wantsInteractions() bool {
	return s.AcceptsInteractions || s.ServiceDID == discoverService
}

// sourceInteractions are the interactions to send on to a filtered feed's source: those with posts
// it served (whose feed context carries its request ID), with the source's own feed context and
// request ID put back. Interactions with the sign-in post, or anything else not from the source,
// are left out.
func sourceInteractions(in []Interaction) []Interaction {
	out := make([]Interaction, 0, len(in))
	for _, it := range in {
		if !strings.HasPrefix(it.FeedContext, contextPrefix) || it.Item == "" || it.Event == "" {
			continue
		}
		reqID, ctx := unwrapContext(it.FeedContext)
		out = append(out, Interaction{Item: it.Item, Event: it.Event, FeedContext: ctx, ReqID: reqID})
	}
	return out
}

// Forward sends the viewer's interactions with the filtered feed f's posts on to its source, with
// a token the viewer's own server signs, in the background: the viewer's app isn't kept waiting.
func (ff *FilteredFeeds) Forward(f Feed, viewer string, in []Interaction) {
	src, ok := ff.Sources.Get(f.Filtered.Source)
	if !ok || !src.wantsInteractions() {
		return
	}
	out := sourceInteractions(in)
	if len(out) == 0 {
		return
	}
	if ff.forwards != nil {
		select {
		case ff.forwards <- struct{}{}:
		default: // as many as can be are under way: these are dropped rather than queued without end
			metricForwardedInteractions.WithLabelValues(f.Rkey, "dropped").Add(float64(len(out)))
			return
		}
	}
	go func() {
		if ff.forwards != nil {
			defer func() { <-ff.forwards }()
		}
		ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
		defer cancel()
		result := "ok"
		if err := ff.forward(ctx, src, viewer, out); errors.Is(err, signin.ErrNotConnected) {
			result = "signin"
		} else if err != nil {
			result = "error"
			ff.Log.Info("filtered feeds: sending interactions on failed", "source", src.URI, "viewer", viewer, "err", err)
		}
		metricForwardedInteractions.WithLabelValues(f.Rkey, result).Add(float64(len(out)))
	}()
}

func (ff *FilteredFeeds) forward(ctx context.Context, src FeedSource, viewer string, out []Interaction) error {
	token, err := ff.Tokens.Token(ctx, viewer, src.Audience(), signin.InteractionsMethod)
	if err != nil {
		return err
	}
	body, err := json.Marshal(map[string]any{"feed": src.URI, "interactions": out})
	if err != nil {
		return err
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, src.Endpoint+"/xrpc/app.bsky.feed.sendInteractions", strings.NewReader(string(body)))
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Content-Type", "application/json")
	resp, err := ff.HTTP.Do(req)
	if err != nil {
		return err
	}
	defer resp.Body.Close()
	b, _ := io.ReadAll(io.LimitReader(resp.Body, 4096))
	if resp.StatusCode != http.StatusOK {
		return fmt.Errorf("status %d: %s", resp.StatusCode, clip(string(b), 300))
	}
	return nil
}

// sourcePage is a page of a source feed's skeleton.
type sourcePage struct {
	Feed []struct {
		Post        string          `json:"post"`
		Reason      json.RawMessage `json:"reason,omitempty"`
		FeedContext string          `json:"feedContext,omitempty"`
	} `json:"feed"`
	Cursor string `json:"cursor,omitempty"`
	ReqID  string `json:"reqId,omitempty"`
}

// passedHeaders are the viewer's request headers passed on to the source, as Bluesky's own servers
// pass them to a feed.
var passedHeaders = []string{"Accept-Language", "X-Bsky-Topics"}

func (ff *FilteredFeeds) fetch(ctx context.Context, feed string, src FeedSource, token, cursor string, limit int, headers http.Header) (sourcePage, error) {
	q := url.Values{"feed": {src.URI}, "limit": {fmt.Sprint(min(limit, maxLimit))}}
	if cursor != "" {
		q.Set("cursor", cursor)
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, src.Endpoint+"/xrpc/app.bsky.feed.getFeedSkeleton?"+q.Encode(), nil)
	if err != nil {
		return sourcePage{}, err
	}
	req.Header.Set("Authorization", "Bearer "+token)
	req.Header.Set("Accept", "application/json")
	for _, h := range passedHeaders {
		if v := headers.Get(h); v != "" {
			req.Header.Set(h, v)
		}
	}
	start := ff.now()
	resp, err := ff.HTTP.Do(req)
	if err != nil {
		metricFilteredSource.WithLabelValues(feed, "error").Inc()
		return sourcePage{}, fmt.Errorf("%w: %v", errSourceFailed, err)
	}
	defer resp.Body.Close()
	metricFilteredSourceSeconds.WithLabelValues(feed).Observe(ff.now().Sub(start).Seconds())
	body, err := io.ReadAll(io.LimitReader(resp.Body, maxSourceBody))
	if err != nil {
		return sourcePage{}, fmt.Errorf("%w: %v", errSourceFailed, err)
	}
	metricFilteredSource.WithLabelValues(feed, fmt.Sprint(resp.StatusCode)).Inc()
	if resp.StatusCode != http.StatusOK {
		// A source that refuses the viewer's token is worth knowing about: it may want another
		// audience than the one the token names.
		ff.Log.Warn("filtered feeds: the source feed refused a request", "source", src.URI, "status", resp.StatusCode,
			"body", clip(string(body), 300))
		return sourcePage{}, fmt.Errorf("%w: status %d", errSourceFailed, resp.StatusCode)
	}
	var page sourcePage
	if err := json.Unmarshal(body, &page); err != nil {
		return sourcePage{}, fmt.Errorf("%w: not a feed skeleton: %v", errSourceFailed, err)
	}
	return page, nil
}
