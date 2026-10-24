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
func newFiltered(ctx context.Context, s *settings, store *feedgen.Store, feeds *feedgen.Feeds, leftOut feedgen.LeftOutSink, log *slog.Logger) (*feedgen.FilteredFeeds, signin.Connecter, error) {
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

	var connect signin.Connecter
	var tokens feedgen.TokenSource = noTokens{}
	secret := os.Getenv("FEEDGEN_FILTER_SECRET")
	switch {
	case secret == "":
		log.Warn("FEEDGEN_FILTER_SECRET is not set: nobody can sign in for the filtered feeds, so they show only the sign-in post")
	default:
		// The sign-in asks permission for exactly the services the sources name, so it can't be made
		// until they have been looked up, and that needs the network. It can be down for a while when
		// the service starts (a machine that has just booted may find no DNS at all for minutes), and
		// a sign-in made then would have nothing to ask permission for: nobody could sign in until
		// the next restart. So it is made in the background, as soon as the sources are found: until
		// then the routes answer 503 and the feeds show the sign-in post, which is what they show a
		// viewer who hasn't signed in anyway.
		waiting := &signin.Deferred{Wait: 10 * time.Second, Log: log}
		waiting.Build = func(ctx context.Context) (*signin.Connector, error) {
			audiences := sources.Audiences()
			if len(audiences) == 0 {
				return nil, nil // Sources is still looking them up, and looks again every minute
			}
			if len(audiences) < len(uris) {
				log.Error("some filtered feeds' sources weren't found: the sign-in doesn't ask permission for them until the service restarts",
					"found", audiences)
			}
			c, err := signin.NewConnector(signin.ConnectConfig{
				Origin: "https://" + s.hostname, ClientName: signInClientName, Secret: []byte(secret),
				Directory: &atmosidentity.Directory{Resolver: &atmosidentity.DefaultResolver{}, Cache: atmosidentity.NewLRUCache(10_000, time.Hour)},
				Audiences: audiences, Store: store, Log: log,
			})
			if err != nil {
				return nil, err
			}
			feedgen.SetFilteredSignInReady(true)
			log.Info("filtered feeds: sign-in on", "audiences", audiences)
			return c, nil
		}
		go waiting.Run(ctx)
		connect, tokens = waiting, waiting
	}
	ff := feedgen.NewFilteredFeeds(sources, feedgen.NewScoreCache(store), tokens, log)
	ff.SignInPost = signInPost
	ff.LeftOut = leftOut
	return ff, connect, nil
}

// newFilteredAPI sets up the page at /filtered's API.
func newFilteredAPI(s *settings, signIn *signin.Handler, connect signin.Connecter, store *feedgen.Store, feeds *feedgen.Feeds,
	ff *feedgen.FilteredFeeds, paths map[string]bool, log *slog.Logger) *feedgen.FilteredAPI {
	connected := func(context.Context, string) (bool, error) { return false, nil }
	if connect != nil {
		connected = connect.Connected
	}
	return &feedgen.FilteredAPI{
		Viewer: signIn.Viewer, Connected: connected, Feeds: feeds, Sources: ff.Sources, Store: store, Service: ff,
		OwnerDID: s.owner.String(), Paths: paths, Origin: "https://" + s.hostname,
		// Reading or saving filters is a small query or two: ten at once, then one every two seconds.
		Edits: feedgen.NewIPLimiter(2*time.Second, 10), Log: log,
	}
}
