package main

import (
	"context"
	"log/slog"
	"os"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	atmosidentity "github.com/jcalabro/atmos/identity"

	"github.com/haileyok/topic-feed/internal/feedgen"
	"github.com/haileyok/topic-feed/internal/signin"
)

// sourceTries is how many times the filtered feeds' sources are looked up before the service
// starts without the ones not found (the sign-in can only ask permission for those found).
const sourceTries = 3

// noTokens is the token source while the filtered feeds' sign-in is off: nobody has one.
type noTokens struct{}

func (noTokens) Token(context.Context, string, string, string) (string, error) {
	return "", signin.ErrNotConnected
}

// newFiltered sets up the filtered feeds of the config, if there are any (nil, nil otherwise): their
// sources, the sign-in that keeps viewers' sign-ins (nil when FEEDGEN_FILTER_SECRET is unset, and
// then every viewer gets the sign-in post), and the service that serves them.
func newFiltered(ctx context.Context, s *settings, store *feedgen.Store, feeds *feedgen.Feeds, leftOut feedgen.LeftOutSink, log *slog.Logger) (*feedgen.FilteredFeeds, *signin.Connector, error) {
	var uris []string
	for _, f := range s.cfg.Feeds {
		if f.Filtered != nil {
			uris = append(uris, f.Filtered.Source)
		}
	}
	if len(uris) == 0 {
		return nil, nil, nil
	}
	sources := feedgen.NewSources(identity.DefaultDirectory(), log)
	for try := 1; try <= sourceTries; try++ {
		if sources.LookupAll(ctx, uris) == nil {
			break
		}
		if try < sourceTries {
			select {
			case <-ctx.Done():
				return nil, nil, ctx.Err()
			case <-time.After(5 * time.Second):
			}
		}
	}
	go sources.Run(ctx, uris)

	signInPost := os.Getenv("FEEDGEN_FILTER_SIGNIN_POST")
	if err := checkPostURI("FEEDGEN_FILTER_SIGNIN_POST", signInPost); err != nil {
		return nil, nil, err
	}
	if signInPost == "" {
		log.Warn("FEEDGEN_FILTER_SIGNIN_POST is not set: viewers who haven't signed in for the filtered feeds see an empty feed (make feeds-welcome FOR=filtered creates the post)")
	}

	var connector *signin.Connector
	var tokens feedgen.TokenSource = noTokens{}
	secret := os.Getenv("FEEDGEN_FILTER_SECRET")
	audiences := sources.Audiences()
	switch {
	case secret == "":
		log.Warn("FEEDGEN_FILTER_SECRET is not set: nobody can sign in for the filtered feeds, so they show only the sign-in post")
	case len(audiences) == 0:
		log.Error("none of the filtered feeds' sources could be found: nobody can sign in for them until the service restarts")
	default:
		if len(audiences) < len(uris) {
			log.Error("some filtered feeds' sources weren't found: the sign-in doesn't ask permission for them until the service restarts",
				"found", audiences)
		}
		var err error
		connector, err = signin.NewConnector(signin.ConnectConfig{
			Origin: "https://" + s.hostname, ClientName: signInClientName, Secret: []byte(secret),
			Directory: &atmosidentity.Directory{Resolver: &atmosidentity.DefaultResolver{}, Cache: atmosidentity.NewLRUCache(10_000, time.Hour)},
			Audiences: audiences, Store: store, Log: log,
		})
		if err != nil {
			return nil, nil, err
		}
		tokens = connector
		log.Info("filtered feeds: sign-in on", "audiences", audiences)
	}
	ff := feedgen.NewFilteredFeeds(sources, store, tokens, log)
	ff.SignInPost = signInPost
	ff.LeftOut = leftOut
	return ff, connector, nil
}

// newFilteredAPI sets up the page at /filtered's API.
func newFilteredAPI(s *settings, signIn *signin.Handler, connector *signin.Connector, store *feedgen.Store, feeds *feedgen.Feeds,
	ff *feedgen.FilteredFeeds, paths map[string]bool, log *slog.Logger) *feedgen.FilteredAPI {
	connected := func(context.Context, string) (bool, error) { return false, nil }
	if connector != nil {
		connected = connector.Connected
	}
	return &feedgen.FilteredAPI{
		Viewer: signIn.Viewer, Connected: connected, Feeds: feeds, Sources: ff.Sources, Store: store, Service: ff,
		OwnerDID: s.owner.String(), Paths: paths, Origin: "https://" + s.hostname,
		// Reading or saving filters is a small query or two: ten at once, then one every two seconds.
		Edits: feedgen.NewIPLimiter(2*time.Second, 10), Log: log,
	}
}
