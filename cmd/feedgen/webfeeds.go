package main

import (
	"log/slog"
	"time"

	atmosidentity "github.com/jcalabro/atmos/identity"

	"github.com/haileyok/topic-feed/internal/feedgen"
	"github.com/haileyok/topic-feed/internal/signin"
)

// newFeedsAPI sets up what a signed-in person can do with their own feeds. owner is the DID of the
// service owner, who has no limits on theirs; origin is https://host.
func newFeedsAPI(owner, serviceDID, origin string, signIn *signin.Handler, store *feedgen.Store, feeds *feedgen.Feeds,
	paths map[string]bool, log *slog.Logger) *feedgen.FeedsAPI {
	return &feedgen.FeedsAPI{
		Viewer: signIn.Viewer, Owner: owner, ServiceDID: serviceDID, Store: store, Feeds: feeds, Paths: paths,
		Limits: feedgen.DefaultUserLimits, Origin: origin,
		// Reading or changing feeds is a database query or two: ten at once, then one every two seconds.
		Edits: feedgen.NewIPLimiter(2*time.Second, 10),
		Log:   log,
	}
}

// newResolveHandleAPI sets up the handle lookups of the page that publishes feeds: a browser can't
// look up a handle's DNS record, and asking Bluesky's servers would tell them who signs in here.
// A lookup reaches out to other servers, so one address gets ten at once and then one a second.
func newResolveHandleAPI(log *slog.Logger) *feedgen.ResolveHandleAPI {
	dir := &atmosidentity.Directory{
		Resolver: &atmosidentity.DefaultResolver{},
		Cache:    atmosidentity.NewLRUCache(10_000, time.Hour),
	}
	return &feedgen.ResolveHandleAPI{Resolve: didOfHandle(dir), Limit: feedgen.NewIPLimiter(time.Second, 10), Log: log}
}
