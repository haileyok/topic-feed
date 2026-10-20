package feedgen

import (
	"context"
	"fmt"
	"log/slog"
	"net/url"
	"slices"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
)

// FeedSource is a feed a filtered feed shows the posts of: where its generator record says it is
// served from.
type FeedSource struct {
	URI string // the at:// URI of its generator record
	// ServiceDID is the DID its record names (its `did`): the audience of the tokens it is sent.
	ServiceDID string
	// Endpoint is that service's #bsky_fg address, https://host.
	Endpoint string
	// AcceptsInteractions is the record's acceptsInteractions: whether it wants what viewers did.
	AcceptsInteractions bool
	// DisplayName is the record's name for the feed.
	DisplayName string
}

// Audience is what a token for the source names as its audience: the service DID with its
// #bsky_fg service, which is the form a sign-in permission for one service must take.
func (s FeedSource) Audience() string { return s.ServiceDID + "#bsky_fg" }

// LookupSource finds where the feed with the generator record uri is served from, through dir.
func LookupSource(ctx context.Context, dir identity.Directory, uri string) (FeedSource, error) {
	u, err := syntax.ParseATURI(uri)
	if err != nil {
		return FeedSource{}, err
	}
	repo, err := u.Authority().AsDID()
	if err != nil {
		return FeedSource{}, fmt.Errorf("%s: the feed's account must be a DID", uri)
	}
	owner, err := dir.LookupDID(ctx, repo)
	if err != nil {
		return FeedSource{}, fmt.Errorf("resolve %s: %w", repo, err)
	}
	pds := owner.PDSEndpoint()
	if pds == "" {
		return FeedSource{}, fmt.Errorf("%s has no PDS", repo)
	}
	var rec struct {
		Value struct {
			DID                 string `json:"did"`
			AcceptsInteractions bool   `json:"acceptsInteractions"`
			DisplayName         string `json:"displayName"`
		} `json:"value"`
	}
	if err := atclient.NewAPIClient(pds).Get(ctx, syntax.NSID("com.atproto.repo.getRecord"), map[string]any{
		"repo": repo.String(), "collection": generatorCollection, "rkey": u.RecordKey().String(),
	}, &rec); err != nil {
		return FeedSource{}, fmt.Errorf("read the record of %s: %w", uri, err)
	}
	svc, err := syntax.ParseDID(rec.Value.DID)
	if err != nil {
		return FeedSource{}, fmt.Errorf("the record of %s names no service DID: %q", uri, rec.Value.DID)
	}
	ident, err := dir.LookupDID(ctx, svc)
	if err != nil {
		return FeedSource{}, fmt.Errorf("resolve the service %s: %w", svc, err)
	}
	endpoint := strings.TrimRight(ident.GetServiceEndpoint("bsky_fg"), "/")
	if e, err := url.Parse(endpoint); endpoint == "" || err != nil || e.Scheme != "https" || e.Host == "" || e.Path != "" {
		return FeedSource{}, fmt.Errorf("the service %s has no https #bsky_fg address (got %q)", svc, endpoint)
	}
	return FeedSource{URI: uri, ServiceDID: svc.String(), Endpoint: endpoint, AcceptsInteractions: rec.Value.AcceptsInteractions,
		DisplayName: rec.Value.DisplayName}, nil
}

// Sources keeps where the filtered feeds' source feeds are served from, looked up when the service
// starts and again every so often (a feed can move).
type Sources struct {
	// Lookup finds a source; LookupSource with a directory, in the service.
	Lookup func(ctx context.Context, uri string) (FeedSource, error)
	Log    *slog.Logger

	mu    sync.RWMutex
	found map[string]FeedSource
}

// NewSources makes Sources that look up through dir.
func NewSources(dir identity.Directory, log *slog.Logger) *Sources {
	return &Sources{Lookup: func(ctx context.Context, uri string) (FeedSource, error) { return LookupSource(ctx, dir, uri) },
		Log: log, found: map[string]FeedSource{}}
}

// Get is where the source with the generator record uri is served from, once it has been found.
func (s *Sources) Get(uri string) (FeedSource, bool) {
	s.mu.RLock()
	defer s.mu.RUnlock()
	src, ok := s.found[uri]
	return src, ok
}

// Audiences are the audiences of every source found, sorted: what the sign-in for filtered feeds
// asks permission to send requests to.
func (s *Sources) Audiences() []string {
	s.mu.RLock()
	defer s.mu.RUnlock()
	var out []string
	for _, src := range s.found {
		if !slices.Contains(out, src.Audience()) {
			out = append(out, src.Audience())
		}
	}
	slices.Sort(out)
	return out
}

// LookupAll looks up every uri now, keeping what was found before for any that fail. It returns
// the first error.
func (s *Sources) LookupAll(ctx context.Context, uris []string) error {
	var first error
	for _, uri := range uris {
		lctx, cancel := context.WithTimeout(ctx, 15*time.Second)
		src, err := s.Lookup(lctx, uri)
		cancel()
		if err != nil {
			s.Log.Warn("filtered feeds: looking up a source feed failed", "source", uri, "err", err)
			if first == nil {
				first = err
			}
			continue
		}
		s.mu.Lock()
		if old, had := s.found[uri]; had && old != src {
			s.Log.Info("filtered feeds: a source feed moved", "source", uri, "was", old.Endpoint, "now", src.Endpoint)
		}
		s.found[uri] = src
		s.mu.Unlock()
	}
	return first
}

// Run looks the sources up again every so often until ctx ends: every minute while any is missing,
// else every hour.
func (s *Sources) Run(ctx context.Context, uris []string) {
	for {
		wait := time.Hour
		if s.LookupAll(ctx, uris) != nil {
			wait = time.Minute
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}
