package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"time"

	"github.com/jcalabro/atmos"
	atmosidentity "github.com/jcalabro/atmos/identity"

	"github.com/haileyok/topic-feed/internal/feedgen"
	"github.com/haileyok/topic-feed/internal/labelpolicy"
	"github.com/haileyok/topic-feed/internal/signin"
	"github.com/haileyok/topic-feed/internal/taxonomy"
)

const (
	// publicAppView answers questions about public posts without signing in.
	publicAppView = "https://public.api.bsky.app"
	// maxRemoteBody bounds what is read of Bluesky's answer about one post.
	maxRemoteBody = 1 << 20
)

// newInspectAPI sets up the post inspector, which only owner may use. handle finds the handle of
// an account for display (the one the sign-in pages use).
func newInspectAPI(owner string, signIn *signin.Handler, store *feedgen.Store, feeds *feedgen.Feeds, policy *labelpolicy.Policy,
	tax *taxonomy.Taxonomy, handle func(ctx context.Context, did string) string, log *slog.Logger) *feedgen.InspectAPI {
	dir := &atmosidentity.Directory{
		Resolver: &atmosidentity.DefaultResolver{},
		Cache:    atmosidentity.NewLRUCache(1_000, time.Hour),
	}
	return &feedgen.InspectAPI{
		Viewer:  signIn.Viewer,
		Owner:   owner,
		Source:  store,
		Feeds:   feeds,
		Policy:  policy,
		Names:   feedgen.TopicNames(tax),
		Resolve: didOfHandle(dir),
		Handle:  handle,
		Remote:  &appViewPosts{BaseURL: publicAppView, Client: &http.Client{Timeout: 5 * time.Second}},
		// Each answer reads the database several times: ten at once, then one every two seconds.
		Limit: feedgen.NewIPLimiter(2*time.Second, 10),
		Log:   log,
	}
}

// didOfHandle finds the DID of a handle for the inspector. A handle that names no account (or
// whose account doesn't name it back) is feedgen.ErrNoSuchHandle; any other failure is returned as
// it came, so that trouble reaching the network isn't taken for a mistyped handle.
func didOfHandle(dir *atmosidentity.Directory) func(ctx context.Context, handle string) (string, error) {
	return func(ctx context.Context, handle string) (string, error) {
		h, err := atmos.ParseHandle(handle)
		if err != nil {
			return "", feedgen.ErrNoSuchHandle // not a handle at all
		}
		id, err := dir.LookupHandle(ctx, h)
		switch {
		case err == nil:
			if id.Handle == atmos.HandleInvalid {
				return "", feedgen.ErrNoSuchHandle // the account the handle points to doesn't claim it
			}
			return string(id.DID), nil
		case errors.Is(err, atmosidentity.ErrHandleNotFound) && ctx.Err() == nil:
			return "", feedgen.ErrNoSuchHandle
		}
		return "", err
	}
}

// appViewPosts asks Bluesky's public AppView what it says about a post, to explain a post we don't
// hold. It makes one try: the inspector says what it knows without it if it fails.
type appViewPosts struct {
	BaseURL string
	Client  *http.Client
}

// Lookup is feedgen.RemoteSource.
func (a *appViewPosts) Lookup(ctx context.Context, uri string) (feedgen.RemotePost, error) {
	endpoint := strings.TrimRight(a.BaseURL, "/") + "/xrpc/app.bsky.feed.getPosts?" + url.Values{"uris": {uri}}.Encode()
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, endpoint, nil)
	if err != nil {
		return feedgen.RemotePost{}, err
	}
	req.Header.Set("Accept", "application/json")
	req.Header.Set("User-Agent", "topic-feeds-inspector")
	resp, err := a.Client.Do(req)
	if err != nil {
		return feedgen.RemotePost{}, err
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return feedgen.RemotePost{}, fmt.Errorf("getPosts: http %d", resp.StatusCode)
	}
	var body struct {
		Posts []struct {
			URI    string `json:"uri"`
			Record struct {
				Reply     json.RawMessage `json:"reply"`
				Langs     []string        `json:"langs"`
				CreatedAt string          `json:"createdAt"`
			} `json:"record"`
		} `json:"posts"`
	}
	if err := json.NewDecoder(io.LimitReader(resp.Body, maxRemoteBody)).Decode(&body); err != nil {
		return feedgen.RemotePost{}, fmt.Errorf("getPosts: %w", err)
	}
	for _, p := range body.Posts {
		if p.URI != uri {
			continue
		}
		out := feedgen.RemotePost{Found: true, Langs: p.Record.Langs}
		out.IsReply = len(p.Record.Reply) > 0 && string(p.Record.Reply) != "null"
		out.CreatedAt, _ = time.Parse(time.RFC3339, p.Record.CreatedAt)
		return out, nil
	}
	return feedgen.RemotePost{}, nil
}
