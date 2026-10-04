package signin

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/jcalabro/atmos"
	"github.com/jcalabro/atmos/identity"
	"github.com/jcalabro/atmos/oauth"
	"github.com/jcalabro/gt"
)

// ErrBadAccount means what the person typed is not a handle or DID.
var ErrBadAccount = errors.New("that is not a handle or DID")

// Authenticator signs people in to Bluesky accounts. Atmos is the real one; tests use a fake.
type Authenticator interface {
	// Start begins signing in the account (a handle or DID). It returns where to send the
	// person's browser, and the state value that will come back with them.
	Start(ctx context.Context, account string) (redirect, state string, err error)
	// Finish completes the login the person returns from and returns the DID of the account
	// they proved they have. iss is the authorization server that sent them back.
	Finish(ctx context.Context, code, state, iss string) (did string, err error)
}

// AtmosConfig is how the real sign-in is set up.
type AtmosConfig struct {
	// Origin is where this service is served from, like https://feeds.example.com. Sign-in
	// needs an https one; a loopback http one works for trying it out locally.
	Origin string
	// ClientName is what people see on their own server's consent page.
	ClientName string
	// Directory resolves handles and DIDs. Use one that verifies handles both ways.
	Directory *identity.Directory
	// HTTPClient makes every request to the person's server; nil: the library's, which
	// refuses to reach private addresses. Tests aim it at fake servers.
	HTTPClient *http.Client
	Log        *slog.Logger
	// Now is the clock; nil: time.Now.
	Now func() time.Time
}

// ClientMetadataFor is the OAuth client metadata for a service at origin: a public client
// (no secret to look after) that only asks to know who the person is.
func ClientMetadataFor(origin, name string) oauth.ClientMetadata {
	return oauth.ClientMetadata{
		ClientID:                origin + "/oauth/client-metadata.json",
		ApplicationType:         "web",
		GrantTypes:              []string{"authorization_code", "refresh_token"},
		Scope:                   "atproto",
		ResponseTypes:           []string{"code"},
		RedirectURIs:            []string{origin + "/oauth/callback"},
		DPoPBoundAccessTokens:   true,
		TokenEndpointAuthMethod: "none",
		ClientName:              name,
		ClientURI:               origin,
	}
}

// Atmos signs people in with atproto OAuth, through the atmos library.
type Atmos struct {
	client   *oauth.Client
	sessions *sessionStore
	states   *stateStore
	redirect string
	log      *slog.Logger
}

// NewAtmos prepares sign-in for a service at cfg.Origin.
func NewAtmos(cfg AtmosConfig) (*Atmos, error) {
	if err := checkOrigin(cfg.Origin); err != nil {
		return nil, err
	}
	if cfg.Directory == nil {
		return nil, errors.New("sign-in needs an identity directory")
	}
	if cfg.ClientName == "" {
		cfg.ClientName = "Feeds"
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	now := cfg.Now
	if now == nil {
		now = time.Now
	}
	a := &Atmos{sessions: newSessionStore(), states: newStateStore(pendingTTL, maxPending, now), log: cfg.Log}
	meta := ClientMetadataFor(cfg.Origin, cfg.ClientName)
	a.redirect = meta.RedirectURIs[0]
	a.client = &oauth.Client{
		ClientMetadata: meta,
		Identity:       cfg.Directory,
		SessionStore:   a.sessions,
		StateStore:     a.states,
	}
	if cfg.HTTPClient != nil {
		a.client.HTTPClient = gt.Some(cfg.HTTPClient)
	}
	if err := a.client.ValidateClientAuth(); err != nil {
		return nil, err
	}
	return a, nil
}

// Metadata is the client metadata document to serve at the client ID.
func (a *Atmos) Metadata() oauth.ClientMetadata { return a.client.ClientMetadata }

func (a *Atmos) Start(ctx context.Context, account string) (string, string, error) {
	if _, err := atmos.ParseATIdentifier(account); err != nil {
		return "", "", fmt.Errorf("%w: %q", ErrBadAccount, account)
	}
	res, err := a.client.Authorize(ctx, oauth.AuthorizeOptions{Input: account, RedirectURI: a.redirect})
	if err != nil {
		return "", "", err // wraps ErrBusy when too many logins are in progress
	}
	return res.URL, res.State, nil
}

func (a *Atmos) Finish(ctx context.Context, code, state, iss string) (string, error) {
	sess, err := a.client.Callback(ctx, oauth.CallbackParams{Code: code, State: state, Iss: iss})
	if err != nil {
		return "", err
	}
	did := sess.TokenSet.Sub
	// We only wanted to know who this is. Revoke the tokens, and drop them whether or not
	// the server heard us.
	if err := a.client.SignOut(ctx, did, sess.SessionID); err != nil {
		a.log.Warn("signing out of a finished login failed", "did", did, "err", err)
	}
	_ = a.sessions.DeleteSession(ctx, did, sess.SessionID)
	if _, err := syntax.ParseDID(did); err != nil {
		return "", fmt.Errorf("the server named an account that is not a DID: %q", did)
	}
	return did, nil
}

// checkOrigin accepts an https origin, or an http one on a loopback host (for trying it out),
// with no path.
func checkOrigin(origin string) error {
	rest, ok := strings.CutPrefix(origin, "https://")
	if !ok {
		rest, ok = strings.CutPrefix(origin, "http://")
		host := strings.SplitN(rest, ":", 2)[0]
		if !ok || (host != "localhost" && host != "127.0.0.1" && host != "[::1]") {
			return fmt.Errorf("the origin %q must start with https:// (http:// only for localhost)", origin)
		}
	}
	if rest == "" || strings.ContainsAny(rest, "/?#@ ") {
		return fmt.Errorf("the origin %q must be a scheme and host only", origin)
	}
	return nil
}
