package signin

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/jcalabro/atmos/oauth"
)

// ErrNotReady means the filtered feeds' sign-in can't be used yet: the services its sources are
// served from haven't been found, so there is nothing to ask permission for.
var ErrNotReady = errors.New("the filtered feeds' sign-in isn't ready")

// The two signs in the routes and the filtered feeds are given: one made from services that are
// known, and one that will be.
var (
	_ Connecter = (*Connector)(nil)
	_ Connecter = (*Deferred)(nil)
)

// Connecter is the sign-in for the filtered feeds as the pages and the filtered feeds use it: a
// Connector, or a Deferred one that is waiting for its sources' services.
type Connecter interface {
	Authenticator
	// Metadata is the client metadata to serve at the client ID.
	Metadata() oauth.ClientMetadata
	// Ready says whether the sign-in can be used: one that is still waiting for its sources'
	// services can't, and the routes that use it answer 503.
	Ready() bool
	// Connected reports whether the viewer has a sign-in kept, without asking their server.
	Connected(ctx context.Context, did string) (bool, error)
	// Disconnect revokes the viewer's sign-in and forgets it.
	Disconnect(ctx context.Context, did string) error
	// Token is a token the viewer's server signed for aud (a did:...#bsky_fg service) and the
	// method lxm, from the cache while it lasts. ErrNotConnected when they have no sign-in.
	Token(ctx context.Context, did, aud, lxm string) (string, error)
}

// Deferred is the filtered feeds' sign-in before its sources' services are known. The sign-in asks
// permission for exactly the services those sources name, so it can't be made until they have been
// looked up, and a lookup needs the network. That network can be down for a while when the service
// starts (a machine that has just booted finds no DNS at all, say), and a sign-in made then would
// have nothing to ask permission for: it would be off for as long as the process lives, and nobody
// could sign in for the filtered feeds. So the routes answer 503 until it is made, and Run keeps
// trying while the sources are looked up again.
type Deferred struct {
	// Build makes the sign-in from what is known now. It returns nil, nil while the sources'
	// services aren't known yet, so that Run tries again; any error is a problem with the service's
	// own settings (the secret, say), which is reported and tried again too.
	Build func(ctx context.Context) (*Connector, error)
	// Wait is how long to wait between tries. Zero is a minute.
	Wait time.Duration
	Log  *slog.Logger

	made atomic.Pointer[Connector]
}

// Run builds the sign-in, and keeps trying until it has been made or ctx ends. The service starts
// it in the background: the routes answering 503 for a moment is better than waiting for DNS at
// startup, which could be a long time.
func (d *Deferred) Run(ctx context.Context) {
	wait := d.Wait
	if wait <= 0 {
		wait = time.Minute
	}
	log := d.Log
	if log == nil {
		log = slog.Default()
	}
	var reported string
	for {
		c, err := d.Build(ctx)
		switch {
		case ctx.Err() != nil:
			return
		case err != nil:
			// Worth saying once, and again when it changes: this is the service's own settings,
			// not something a retry fixes by itself, and the routes answering 503 say the same.
			if msg := err.Error(); msg != reported {
				reported = msg
				log.Error("filtered feeds: making the sign-in failed: nobody can sign in until it can be, and the routes answer 503", "err", err)
			}
		case c != nil:
			d.made.Store(c)
			return
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
	}
}

// Ready says whether the sign-in has been made.
func (d *Deferred) Ready() bool { return d.made.Load() != nil }

// get is the sign-in, or nil before it is made.
func (d *Deferred) get() *Connector { return d.made.Load() }

// Metadata is the client metadata of the sign-in made, and the zero one before that: the routes
// answer 503 until it is made, so nothing reads it.
func (d *Deferred) Metadata() oauth.ClientMetadata {
	if c := d.get(); c != nil {
		return c.Metadata()
	}
	return oauth.ClientMetadata{}
}

// Start begins a sign-in, which can't be started before the sign-in for the filtered feeds is made.
func (d *Deferred) Start(ctx context.Context, account string) (redirect, state string, err error) {
	if c := d.get(); c != nil {
		return c.Start(ctx, account)
	}
	return "", "", ErrNotReady
}

// Finish completes a sign-in.
func (d *Deferred) Finish(ctx context.Context, code, state, iss string) (string, error) {
	if c := d.get(); c != nil {
		return c.Finish(ctx, code, state, iss)
	}
	return "", ErrNotReady
}

// Connected says whether the viewer has a sign-in kept: before the sign-in for the filtered feeds
// is made, nobody has one, and the feeds show the sign-in post, which is what a viewer who hasn't
// signed in sees anyway.
func (d *Deferred) Connected(ctx context.Context, did string) (bool, error) {
	if c := d.get(); c != nil {
		return c.Connected(ctx, did)
	}
	return false, nil
}

// Disconnect forgets the viewer's sign-in. Before the sign-in for the filtered feeds is made there
// is none kept to forget.
func (d *Deferred) Disconnect(ctx context.Context, did string) error {
	if c := d.get(); c != nil {
		return c.Disconnect(ctx, did)
	}
	return nil
}

// Token is a token for a source feed, and there is none before the sign-in for the filtered feeds
// is made: ErrNotConnected, which the filtered feeds answer with the sign-in post.
func (d *Deferred) Token(ctx context.Context, did, aud, lxm string) (string, error) {
	if c := d.get(); c != nil {
		return c.Token(ctx, did, aud, lxm)
	}
	return "", ErrNotConnected
}
