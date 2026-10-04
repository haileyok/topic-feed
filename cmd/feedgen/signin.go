package main

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"time"

	"github.com/jcalabro/atmos"
	atmosidentity "github.com/jcalabro/atmos/identity"

	"github.com/haileyok/topic-feed/internal/feedgen"
	"github.com/haileyok/topic-feed/internal/signin"
	"github.com/haileyok/topic-feed/internal/taxonomy"
)

const (
	// signInClientName is what people see on their own server's page asking them to approve.
	signInClientName = "Hailey's feeds"
	// signInLifetime is how long someone stays signed in on a browser.
	signInLifetime = 30 * 24 * time.Hour
)

// serviceAuthDirectory finds the keys that viewers' credentials are signed with. Handles play
// no part in checking a credential, so they aren't verified, which saves a lookup of each
// viewer's own domain. It has its own cache: a cache shared with a directory that does
// verify handles would hold identities without one.
func serviceAuthDirectory() *atmosidentity.Directory {
	return &atmosidentity.Directory{
		Resolver:               &atmosidentity.DefaultResolver{},
		Cache:                  atmosidentity.NewLRUCache(100_000, time.Hour),
		SkipHandleVerification: true,
	}
}

// newSignIn sets up signing in with Bluesky for the service at origin. The session cookie is
// signed with FEEDGEN_SESSION_SECRET; without it sign-in is off and this returns nil, so
// the service runs without the page. It also returns how to find a handle for a DID, which
// the pages that show who is signed in use too. owner is the DID of the account that runs the
// service: /api/me tells the pages when it is the one signed in.
func newSignIn(origin, owner string, log *slog.Logger) (*signin.Handler, func(ctx context.Context, did string) string, error) {
	secret := os.Getenv("FEEDGEN_SESSION_SECRET")
	if secret == "" {
		return nil, nil, nil
	}
	sessions, err := signin.NewSessions([]byte(secret), signInLifetime)
	if err != nil {
		return nil, nil, fmt.Errorf("FEEDGEN_SESSION_SECRET: %w", err)
	}
	dir := &atmosidentity.Directory{
		Resolver: &atmosidentity.DefaultResolver{},
		Cache:    atmosidentity.NewLRUCache(10_000, time.Hour),
	}
	auth, err := signin.NewAtmos(signin.AtmosConfig{Origin: origin, ClientName: signInClientName, Directory: dir, Log: log})
	if err != nil {
		return nil, nil, err
	}
	// Starting a login makes this service call the account's own server: five at once from one
	// visitor, then one every ten seconds.
	logins := feedgen.NewIPLimiter(10*time.Second, 5)
	handle := handleLookup(dir)
	h, err := signin.New(signin.Config{
		Origin: origin, Auth: auth, Metadata: auth.Metadata(), Sessions: sessions,
		Handle: handle, Owner: owner, Allow: logins.AllowRequest, Log: log,
		// The pages with the sign-in form: signing in comes back to the one it was started from.
		Returns: []string{"/me", "/feeds", "/inspect", "/"},
	})
	if err != nil {
		return nil, nil, err
	}
	return h, handle, nil
}

// newMeAPI sets up what a signed-in viewer can see of their own personal feed, from the
// first personal feed in the config. It returns nil if there is none.
func newMeAPI(cfg *feedgen.Config, tax *taxonomy.Taxonomy, store *feedgen.Store, personal *feedgen.Personal,
	signIn *signin.Handler, origin string, handle func(ctx context.Context, did string) string, log *slog.Logger) *feedgen.MeAPI {
	for _, f := range cfg.Feeds {
		if f.Personal == nil {
			continue
		}
		return &feedgen.MeAPI{
			Viewer:    signIn.Viewer,
			Handle:    handle,
			Interests: &feedgen.InterestsBuilder{Src: store, Cfg: *f.Personal, Names: feedgen.TopicNames(tax)},
			Tunings:   store,
			Origin:    origin,
			Personal:  personal,
			Feed:      f.Rkey,
			Ranking:   f.Ranking,
			Texts:     store,
			// Reading a viewer's likes is several database queries: five at once, then one every
			// two seconds, for each viewer.
			Reads: feedgen.NewIPLimiter(2*time.Second, 5),
			// Reading or saving a tuning is one small query (saving may read their likes again):
			// ten at once, then one every two seconds.
			Edits: feedgen.NewIPLimiter(2*time.Second, 10),
			// A preview assembles a feed from memory. Moving a control previews a moment later, so
			// ten at once and then one a second keeps up with someone dragging sliders.
			Previews: feedgen.NewIPLimiter(time.Second, 10),
			Log:      log,
		}
	}
	return nil
}

// handleLookup finds an account's current handle, if it has one that checks out ("" if not,
// and the page shows the DID).
func handleLookup(dir *atmosidentity.Directory) func(ctx context.Context, did string) string {
	return func(ctx context.Context, did string) string {
		id, err := dir.LookupDID(ctx, atmos.DID(did))
		if err != nil || id.Handle == atmos.HandleInvalid {
			return ""
		}
		return string(id.Handle)
	}
}
