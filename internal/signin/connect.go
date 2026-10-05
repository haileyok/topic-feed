package signin

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hkdf"
	"crypto/rand"
	"crypto/sha256"
	"encoding/json"
	"errors"
	"fmt"
	"hash/fnv"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/gttp"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/jcalabro/atmos"
	"github.com/jcalabro/atmos/crypto"
	"github.com/jcalabro/atmos/identity"
	"github.com/jcalabro/atmos/oauth"
	"github.com/jcalabro/atmos/xrpc"
	"github.com/jcalabro/gt"
)

// The methods the filtered feeds ask permission to call on a viewer's behalf.
const (
	FeedSkeletonMethod = "app.bsky.feed.getFeedSkeleton"
	InteractionsMethod = "app.bsky.feed.sendInteractions"
)

// ErrNotConnected means there is no usable sign-in for the viewer: they never signed in for the
// filtered feeds, signed out, or their sign-in ran out or was revoked.
var ErrNotConnected = errors.New("not signed in for filtered feeds")

const (
	// tokenLife is how long a token for a source feed is asked for (the most is an hour), and
	// tokenSlack how long before it runs out a new one is asked for.
	tokenLife  = 30 * time.Minute
	tokenSlack = 5 * time.Minute
	// noLoginFor is how long "this viewer has no sign-in" is remembered, so feed requests from
	// viewers who never signed in don't each read the database.
	noLoginFor = time.Minute
	// lockStripes serialize the work for one viewer: a sign-in's refresh token can be used once.
	lockStripes = 256
)

// LoginStore keeps viewers' sealed sign-ins for the filtered feeds: one per viewer, the newest
// saved winning. The service's store (feedgen.Store) implements it.
type LoginStore interface {
	// LoadLogin returns the viewer's sealed sign-in, or nil when there is none.
	LoadLogin(ctx context.Context, did string) ([]byte, error)
	SaveLogin(ctx context.Context, did string, sealed []byte) error
	DeleteLogin(ctx context.Context, did string) error
}

// ConnectConfig sets up the sign-in for the filtered feeds.
type ConnectConfig struct {
	// Origin is where this service is served from (https://host).
	Origin     string
	ClientName string
	Directory  *identity.Directory
	// Secret (at least 32 bytes) makes the key the service proves itself to viewers' servers with,
	// and the key their sign-ins are sealed with in the database. Changing it signs everyone out.
	Secret []byte
	// Audiences are the services of the source feeds (did:...#bsky_fg): the sign-in asks permission
	// to send them feed requests and interactions, and nothing else.
	Audiences []string
	Store     LoginStore
	// HTTPClient makes the requests to viewers' servers; nil: the library's, which refuses to
	// reach private addresses.
	HTTPClient *http.Client
	Log        *slog.Logger
	Now        func() time.Time
}

// Connector signs viewers in for the filtered feeds, keeps their sign-ins (sealed), and asks their
// servers for the tokens the filtered feeds send to their sources. It is a confidential OAuth
// client: a public client's sign-ins end after two weeks whatever happens, a confidential client's
// last while they keep being used.
type Connector struct {
	client   *oauth.Client
	states   *stateStore
	sessions *loginSessions
	redirect string
	scope    string
	http     *http.Client
	log      *slog.Logger
	now      func() time.Time

	locks [lockStripes]sync.Mutex
	mu    sync.Mutex
	// tokens are the tokens asked for, by viewer, audience and method; noLogin the viewers seen
	// to have no sign-in, and when.
	tokens  map[tokenKey]cachedToken
	noLogin map[string]time.Time
}

type tokenKey struct{ did, aud, lxm string }

type cachedToken struct {
	token   string
	expires time.Time
}

// ConnectScope is the permission asked for: to send feed requests and interactions to each
// audience. Each is named twice, with its #bsky_fg service (what Bluesky's servers require) and
// as the bare DID (what some other servers compare the requested audience with); a server that
// can't read one form ignores it.
func ConnectScope(audiences []string) string {
	parts := []string{"atproto"}
	for _, aud := range audiences {
		for _, a := range audienceForms(aud) {
			parts = append(parts, "rpc?lxm="+FeedSkeletonMethod+"&lxm="+InteractionsMethod+"&aud="+url.QueryEscape(a))
		}
	}
	return strings.Join(parts, " ")
}

// audienceForms are the forms of an audience a token is asked for in: with its service, then bare.
func audienceForms(aud string) []string {
	if bare, _, ok := strings.Cut(aud, "#"); ok {
		return []string{aud, bare}
	}
	return []string{aud}
}

// ConnectMetadataFor is the client metadata of the filtered feeds' sign-in at origin.
func ConnectMetadataFor(origin, name, scope string, jwks oauth.JWKSet) oauth.ClientMetadata {
	return oauth.ClientMetadata{
		ClientID:                    origin + "/oauth/connect-metadata.json",
		ApplicationType:             "web",
		GrantTypes:                  []string{"authorization_code", "refresh_token"},
		Scope:                       scope,
		ResponseTypes:               []string{"code"},
		RedirectURIs:                []string{origin + "/oauth/connect-callback"},
		DPoPBoundAccessTokens:       true,
		TokenEndpointAuthMethod:     "private_key_jwt",
		TokenEndpointAuthSigningAlg: "ES256",
		JWKS:                        &jwks,
		ClientName:                  name,
		ClientURI:                   origin,
	}
}

// deriveKey is 32 bytes for purpose, from the secret.
func deriveKey(secret []byte, purpose string) ([]byte, error) {
	return hkdf.Key(sha256.New, secret, nil, "topic-feed filtered feeds: "+purpose, 32)
}

// NewConnector prepares the sign-in for the filtered feeds.
func NewConnector(cfg ConnectConfig) (*Connector, error) {
	if err := checkOrigin(cfg.Origin); err != nil {
		return nil, err
	}
	switch {
	case len(cfg.Secret) < 32:
		return nil, errors.New("the secret for filtered feeds' sign-ins must be at least 32 characters")
	case cfg.Directory == nil || cfg.Store == nil:
		return nil, errors.New("the sign-in for filtered feeds needs an identity directory and a store")
	case len(cfg.Audiences) == 0:
		return nil, errors.New("the sign-in for filtered feeds needs at least one source feed's service")
	}
	if cfg.Log == nil {
		cfg.Log = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	if cfg.ClientName == "" {
		cfg.ClientName = "Feeds"
	}
	seed, err := deriveKey(cfg.Secret, "client key")
	if err != nil {
		return nil, err
	}
	key, err := crypto.ParsePrivateP256(seed)
	if err != nil {
		return nil, fmt.Errorf("the secret for filtered feeds' sign-ins makes no usable key: %w", err)
	}
	sealKey, err := deriveKey(cfg.Secret, "seal sign-ins")
	if err != nil {
		return nil, err
	}
	block, err := aes.NewCipher(sealKey)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	// The key's ID names the key, so a new secret is a new key to servers that cached the old one.
	sum := sha256.Sum256(key.PublicKey().Bytes())
	keyID := fmt.Sprintf("filtered-%x", sum[:6])

	c := &Connector{states: newStateStore(pendingTTL, maxPending, cfg.Now), log: cfg.Log, now: cfg.Now,
		scope: ConnectScope(cfg.Audiences), tokens: map[tokenKey]cachedToken{}, noLogin: map[string]time.Time{}}
	c.sessions = &loginSessions{store: cfg.Store, aead: aead, onSave: c.forgetNoLogin}
	probe := &oauth.Client{Key: key, KeyID: keyID}
	jwks, err := probe.PublicJWKS()
	if err != nil {
		return nil, err
	}
	meta := ConnectMetadataFor(cfg.Origin, cfg.ClientName, c.scope, jwks)
	c.redirect = meta.RedirectURIs[0]
	c.client = &oauth.Client{ClientMetadata: meta, Identity: cfg.Directory, SessionStore: c.sessions,
		StateStore: c.states, Key: key, KeyID: keyID}
	c.http = cfg.HTTPClient
	if c.http == nil { // what the library uses by default: it refuses to reach private addresses
		c.http = gttp.New(append(xrpc.ATProtoOpts(30*time.Second), gttp.WithStrictSSRFProtection(), gttp.WithNoProxy())...)
	}
	c.client.HTTPClient = gt.Some(c.http)
	if err := c.client.ValidateClientAuth(); err != nil {
		return nil, err
	}
	return c, nil
}

// Metadata is the client metadata to serve at the client ID.
func (c *Connector) Metadata() oauth.ClientMetadata { return c.client.ClientMetadata }

// Start begins the sign-in of account (a handle or DID), asking for the permission in the metadata.
func (c *Connector) Start(ctx context.Context, account string) (string, string, error) {
	if _, err := atmos.ParseATIdentifier(account); err != nil {
		return "", "", fmt.Errorf("%w: %q", ErrBadAccount, account)
	}
	res, err := c.client.Authorize(ctx, oauth.AuthorizeOptions{Input: account, RedirectURI: c.redirect, Scope: c.scope})
	if err != nil {
		return "", "", err
	}
	return res.URL, res.State, nil
}

// Finish completes the sign-in and keeps it (the library stores it through the session store). A
// sign-in the viewer had before is revoked: only the newest is kept.
func (c *Connector) Finish(ctx context.Context, code, state, iss string) (string, error) {
	sess, err := c.client.Callback(ctx, oauth.CallbackParams{Code: code, State: state, Iss: iss})
	if err != nil {
		return "", err
	}
	did := sess.TokenSet.Sub
	if _, err := syntax.ParseDID(did); err != nil {
		return "", fmt.Errorf("the server named an account that is not a DID: %q", did)
	}
	if prev := c.sessions.replaced(did); prev != nil && prev.SessionID != sess.SessionID {
		oauth.RevokeToken(ctx, prev.TokenSet.RevocationEndpoint, prev.TokenSet.RefreshToken, c.clientAuthFor(), prev.DPoPKey,
			oauth.NewNonceStore(), c.http)
	}
	c.dropTokens(did)
	c.log.Info("filtered feeds: signed in", "did", did, "scope", sess.TokenSet.Scope)
	return did, nil
}

func (c *Connector) clientAuthFor() oauth.ClientAuth {
	return &oauth.ConfidentialClientAuth{ClientID: c.client.ClientMetadata.ClientID, Key: c.client.Key, KeyID: c.client.KeyID}
}

// Connected reports whether the viewer has a sign-in for the filtered feeds. It doesn't ask their
// server whether it still works.
func (c *Connector) Connected(ctx context.Context, did string) (bool, error) {
	sess, err := c.sessions.load(ctx, did)
	return sess != nil, err
}

// Disconnect revokes the viewer's sign-in and forgets it.
func (c *Connector) Disconnect(ctx context.Context, did string) error {
	l := c.lock(did)
	l.Lock()
	defer l.Unlock()
	c.dropTokens(did)
	sess, err := c.sessions.load(ctx, did)
	if err != nil || sess == nil {
		return err
	}
	if err := c.client.SignOut(ctx, did, sess.SessionID); err != nil {
		c.log.Info("filtered feeds: revoking a sign-in failed; forgetting it anyway", "did", did, "err", err)
		return c.sessions.store.DeleteLogin(ctx, did)
	}
	return nil
}

// Token is a token the viewer's server signed for aud (a did:...#bsky_fg service) and the method
// lxm, asked for with their sign-in, from the cache while it lasts. It returns ErrNotConnected when
// there is no sign-in, or it no longer works (it is then forgotten); any other error may pass.
func (c *Connector) Token(ctx context.Context, did, aud, lxm string) (string, error) {
	now := c.now()
	k := tokenKey{did, aud, lxm}
	c.mu.Lock()
	if t, ok := c.tokens[k]; ok && now.Before(t.expires.Add(-tokenSlack)) {
		c.mu.Unlock()
		return t.token, nil
	}
	if at, ok := c.noLogin[did]; ok && now.Sub(at) < noLoginFor {
		c.mu.Unlock()
		return "", ErrNotConnected
	}
	c.mu.Unlock()

	l := c.lock(did)
	l.Lock()
	defer l.Unlock()
	c.mu.Lock() // another request may have asked while this one waited
	if t, ok := c.tokens[k]; ok && now.Before(t.expires.Add(-tokenSlack)) {
		c.mu.Unlock()
		return t.token, nil
	}
	c.mu.Unlock()

	sess, err := c.sessions.load(ctx, did)
	if err != nil {
		return "", err
	}
	if sess == nil {
		c.mu.Lock()
		c.noLogin[did] = now
		c.mu.Unlock()
		return "", ErrNotConnected
	}
	xc, err := c.client.AuthenticatedClient(ctx, did, sess.SessionID)
	if err != nil {
		return "", c.failed(ctx, did, err)
	}
	exp := now.Add(tokenLife)
	var lastErr error
	for _, a := range audienceForms(aud) {
		var out struct {
			Token string `json:"token"`
		}
		err := xc.Query(ctx, "com.atproto.server.getServiceAuth", map[string]any{"aud": a, "lxm": lxm, "exp": exp.Unix()}, &out)
		if err == nil && out.Token != "" {
			c.mu.Lock()
			c.tokens[k] = cachedToken{token: out.Token, expires: exp}
			c.mu.Unlock()
			return out.Token, nil
		}
		if err == nil {
			err = errors.New("the server sent no token")
		}
		lastErr = err
		var xe *xrpc.Error
		if !errors.As(err, &xe) || xe.StatusCode >= 500 || xe.StatusCode == http.StatusUnauthorized {
			break // not a refusal of this form of the audience: the other form won't do better
		}
	}
	return "", c.failed(ctx, did, lastErr)
}

// failed sorts a failure to get a token: a sign-in that no longer works is forgotten and is
// ErrNotConnected; anything else (a server that is down, say) is returned as it is.
func (c *Connector) failed(ctx context.Context, did string, err error) error {
	var oe *oauth.OAuthError
	var xe *xrpc.Error
	gone := errors.Is(err, oauth.ErrNoSession) || errors.Is(err, oauth.ErrNoRefreshToken) ||
		(errors.As(err, &oe) && (oe.Code == "invalid_grant" || oe.Code == "invalid_client" || oe.Code == "unauthorized_client")) ||
		// A server error says nothing about the sign-in, whatever its body names.
		(errors.As(err, &xe) && xe.StatusCode < 500 && (xe.StatusCode == http.StatusUnauthorized || xe.StatusCode == http.StatusForbidden ||
			xe.Name == "InsufficientScope" || xe.Name == "InvalidToken" || xe.Name == "ExpiredToken"))
	if !gone {
		return err
	}
	c.log.Info("filtered feeds: a sign-in no longer works; forgetting it", "did", did, "err", err)
	c.dropTokens(did)
	if derr := c.sessions.store.DeleteLogin(ctx, did); derr != nil {
		c.log.Warn("filtered feeds: forgetting a sign-in failed", "did", did, "err", derr)
	}
	c.mu.Lock()
	c.noLogin[did] = c.now()
	c.mu.Unlock()
	return fmt.Errorf("%w: %v", ErrNotConnected, err)
}

func (c *Connector) dropTokens(did string) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for k := range c.tokens {
		if k.did == did {
			delete(c.tokens, k)
		}
	}
	delete(c.noLogin, did)
	// Bound the caches: expired tokens and old "no sign-in"s go.
	now := c.now()
	if len(c.tokens) > 50_000 {
		for k, t := range c.tokens {
			if !now.Before(t.expires) {
				delete(c.tokens, k)
			}
		}
	}
	if len(c.noLogin) > 50_000 {
		for d, at := range c.noLogin {
			if now.Sub(at) >= noLoginFor {
				delete(c.noLogin, d)
			}
		}
	}
}

func (c *Connector) forgetNoLogin(did string) {
	c.mu.Lock()
	delete(c.noLogin, did)
	c.mu.Unlock()
}

func (c *Connector) lock(did string) *sync.Mutex {
	h := fnv.New32a()
	h.Write([]byte(did))
	return &c.locks[h.Sum32()%lockStripes]
}

// loginSessions is the library's session store, over the database: sessions are sealed (AES-GCM,
// with the viewer's DID as associated data, so a sealed sign-in can't be moved to another viewer).
type loginSessions struct {
	store  LoginStore
	aead   cipher.AEAD
	onSave func(did string)

	mu   sync.Mutex
	prev map[string]*oauth.Session // a viewer's sign-in replaced by a new one, until Finish revokes it
}

func (s *loginSessions) seal(did string, sess *oauth.Session) ([]byte, error) {
	plain, err := json.Marshal(sess)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return nil, err
	}
	return s.aead.Seal(nonce, nonce, plain, []byte(did)), nil
}

func (s *loginSessions) open(did string, sealed []byte) (*oauth.Session, error) {
	n := s.aead.NonceSize()
	if len(sealed) < n {
		return nil, errors.New("sealed sign-in is too short")
	}
	plain, err := s.aead.Open(nil, sealed[:n], sealed[n:], []byte(did))
	if err != nil {
		return nil, errors.New("a sealed sign-in doesn't open (was the secret changed?)")
	}
	var sess oauth.Session
	if err := json.Unmarshal(plain, &sess); err != nil {
		return nil, err
	}
	return &sess, nil
}

// load is the viewer's sign-in, or nil. One that can't be opened counts as none.
func (s *loginSessions) load(ctx context.Context, did string) (*oauth.Session, error) {
	sealed, err := s.store.LoadLogin(ctx, did)
	if err != nil || sealed == nil {
		return nil, err
	}
	sess, err := s.open(did, sealed)
	if err != nil {
		return nil, nil
	}
	return sess, nil
}

func (s *loginSessions) GetSession(ctx context.Context, did, sessionID string) (*oauth.Session, error) {
	sess, err := s.load(ctx, did)
	if err != nil {
		return nil, err
	}
	if sess == nil || sess.SessionID != sessionID {
		return nil, oauth.ErrNoSession
	}
	return sess, nil
}

func (s *loginSessions) SetSession(ctx context.Context, sess *oauth.Session) error {
	did := sess.TokenSet.Sub
	if did == "" || sess.SessionID == "" {
		return errors.New("a sign-in needs a DID and a session ID")
	}
	if old, err := s.load(ctx, did); err == nil && old != nil && old.SessionID != sess.SessionID {
		s.mu.Lock()
		if s.prev == nil {
			s.prev = map[string]*oauth.Session{}
		}
		s.prev[did] = old
		s.mu.Unlock()
	}
	sealed, err := s.seal(did, sess)
	if err != nil {
		return err
	}
	if err := s.store.SaveLogin(ctx, did, sealed); err != nil {
		return err
	}
	if s.onSave != nil {
		s.onSave(did)
	}
	return nil
}

func (s *loginSessions) DeleteSession(ctx context.Context, did, sessionID string) error {
	sess, err := s.load(ctx, did)
	if err != nil {
		return err
	}
	if sess != nil && sess.SessionID != sessionID {
		return nil // a newer sign-in: leave it
	}
	return s.store.DeleteLogin(ctx, did)
}

// replaced takes the sign-in that the viewer's newest one replaced, if any.
func (s *loginSessions) replaced(did string) *oauth.Session {
	s.mu.Lock()
	defer s.mu.Unlock()
	p := s.prev[did]
	delete(s.prev, did)
	return p
}
