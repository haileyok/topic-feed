package signin

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/jcalabro/atmos"
	"github.com/jcalabro/atmos/identity"
	"github.com/jcalabro/atmos/oauth"
)

// fakeResolver answers identity lookups from a table, so no test touches the network for them.
type fakeResolver struct {
	mu      sync.Mutex
	docs    map[string]*identity.DIDDocument
	handles map[string]string
}

func (f *fakeResolver) ResolveDID(_ context.Context, did atmos.DID) (*identity.DIDDocument, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.docs[string(did)]; ok {
		return d, nil
	}
	return nil, fmt.Errorf("no such DID %s", did)
}

func (f *fakeResolver) ResolveHandle(_ context.Context, handle atmos.Handle) (atmos.DID, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	if d, ok := f.handles[string(handle)]; ok {
		return atmos.DID(d), nil
	}
	return "", fmt.Errorf("no such handle %s", handle)
}

// world is one account's own servers: the PDS that holds the account, and the authorization
// server it names, which is where the person signs in and approves.
type world struct {
	t      *testing.T
	pds    *httptest.Server
	as     *httptest.Server
	did    string
	handle string

	hits atomic.Int64

	mu           sync.Mutex
	tokenSub     string // the account the token endpoint says was signed in
	revokeStatus int
	requestN     int
	par          []url.Values
	tokens       []url.Values
	revoked      []string
	challenge    string
}

func newWorld(t *testing.T, handle, did string) *world {
	t.Helper()
	w := &world{t: t, did: did, handle: handle, tokenSub: did, revokeStatus: 200}
	count := func(h http.Handler) http.Handler {
		return http.HandlerFunc(func(rw http.ResponseWriter, r *http.Request) {
			w.hits.Add(1)
			h.ServeHTTP(rw, r)
		})
	}
	w.pds = httptest.NewServer(count(http.HandlerFunc(w.servePDS)))
	w.as = httptest.NewServer(count(http.HandlerFunc(w.serveAS)))
	t.Cleanup(w.pds.Close)
	t.Cleanup(w.as.Close)
	return w
}

func writeJSON(rw http.ResponseWriter, status int, v any) {
	rw.Header().Set("Content-Type", "application/json")
	rw.WriteHeader(status)
	_ = json.NewEncoder(rw).Encode(v)
}

func (w *world) servePDS(rw http.ResponseWriter, r *http.Request) {
	if r.URL.Path != "/.well-known/oauth-protected-resource" {
		http.NotFound(rw, r)
		return
	}
	writeJSON(rw, 200, map[string]any{"resource": w.pds.URL, "authorization_servers": []string{w.as.URL}})
}

func (w *world) serveAS(rw http.ResponseWriter, r *http.Request) {
	_ = r.ParseForm()
	w.mu.Lock()
	defer w.mu.Unlock()
	switch r.URL.Path {
	case "/.well-known/oauth-authorization-server":
		writeJSON(rw, 200, map[string]any{
			"issuer":                                         w.as.URL,
			"authorization_endpoint":                         w.as.URL + "/oauth/authorize",
			"token_endpoint":                                 w.as.URL + "/oauth/token",
			"revocation_endpoint":                            w.as.URL + "/oauth/revoke",
			"pushed_authorization_request_endpoint":          w.as.URL + "/oauth/par",
			"client_id_metadata_document_supported":          true,
			"require_pushed_authorization_requests":          true,
			"authorization_response_iss_parameter_supported": true,
		})
	case "/oauth/par":
		w.par = append(w.par, r.PostForm)
		w.challenge = r.PostForm.Get("code_challenge")
		w.requestN++
		writeJSON(rw, 201, map[string]any{"request_uri": fmt.Sprintf("urn:ietf:params:oauth:request_uri:req-%d", w.requestN), "expires_in": 60})
	case "/oauth/token":
		w.tokens = append(w.tokens, r.PostForm)
		sum := sha256.Sum256([]byte(r.PostForm.Get("code_verifier")))
		if r.PostForm.Get("grant_type") != "authorization_code" || r.PostForm.Get("code") == "" ||
			base64.RawURLEncoding.EncodeToString(sum[:]) != w.challenge || r.Header.Get("DPoP") == "" {
			writeJSON(rw, 400, map[string]string{"error": "invalid_grant"})
			return
		}
		writeJSON(rw, 200, map[string]any{"access_token": "access-1", "refresh_token": "refresh-1", "token_type": "DPoP",
			"expires_in": 300, "scope": "atproto", "sub": w.tokenSub})
	case "/oauth/revoke":
		w.revoked = append(w.revoked, r.PostForm.Get("token"))
		rw.WriteHeader(w.revokeStatus)
	default:
		http.NotFound(rw, r)
	}
}

func (w *world) doc() *identity.DIDDocument {
	return &identity.DIDDocument{ID: w.did, AlsoKnownAs: []string{"at://" + w.handle}, Service: []identity.Service{
		{ID: "#atproto_pds", Type: "AtprotoPersonalDataServer", ServiceEndpoint: w.pds.URL}}}
}

const (
	aliceDID   = "did:plc:alicealicealicealicealic"
	malloryDID = "did:plc:mallorymallorymallorymall"
)

func resolverFor(worlds ...*world) *fakeResolver {
	r := &fakeResolver{docs: map[string]*identity.DIDDocument{}, handles: map[string]string{}}
	for _, w := range worlds {
		r.docs[w.did] = w.doc()
		r.handles[w.handle] = w.did
	}
	return r
}

func newTestAtmos(t *testing.T, res identity.Resolver, client *http.Client) *Atmos {
	t.Helper()
	a, err := NewAtmos(AtmosConfig{Origin: testOrigin, ClientName: "Test feeds", Directory: &identity.Directory{Resolver: res},
		HTTPClient: client, Log: slog.New(slog.NewTextHandler(io.Discard, nil))})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

// loopback lets the library reach the fake servers, which it otherwise refuses to (they
// are on private addresses).
func loopback() *http.Client { return &http.Client{Timeout: 10 * time.Second} }

func TestAtmosSignsAnAccountIn(t *testing.T) {
	alice := newWorld(t, "alice.example.test", aliceDID)
	a := newTestAtmos(t, resolverFor(alice), loopback())
	ctx := context.Background()

	// The person types a handle; we send them to their own authorization server.
	redirect, state, err := a.Start(ctx, "alice.example.test")
	if err != nil {
		t.Fatal(err)
	}
	u, err := url.Parse(redirect)
	if err != nil || !strings.HasPrefix(redirect, alice.as.URL+"/oauth/authorize?") {
		t.Fatalf("redirect %q (%v)", redirect, err)
	}
	if got := u.Query().Get("client_id"); got != testOrigin+"/oauth/client-metadata.json" {
		t.Errorf("client_id %q", got)
	}
	if got := u.Query().Get("request_uri"); got != "urn:ietf:params:oauth:request_uri:req-1" {
		t.Errorf("request_uri %q", got)
	}
	if state == "" || a.states.len() != 1 {
		t.Errorf("state %q, %d pending", state, a.states.len())
	}
	if len(alice.par) != 1 {
		t.Fatalf("%d pushed authorization requests", len(alice.par))
	}
	par := alice.par[0]
	for k, want := range map[string]string{
		"response_type": "code", "code_challenge_method": "S256", "redirect_uri": testOrigin + "/oauth/callback",
		"scope": "atproto", "state": state, "login_hint": "alice.example.test", "client_id": testOrigin + "/oauth/client-metadata.json",
	} {
		if par.Get(k) != want {
			t.Errorf("PAR %s = %q, want %q", k, par.Get(k), want)
		}
	}
	if par.Get("code_challenge") == "" || par.Get("client_assertion") != "" {
		t.Errorf("a public client sends a PKCE challenge and no client assertion: %v", par)
	}

	// They approve, and the server sends them back with a code. We learn who they are.
	did, err := a.Finish(ctx, "the-code", state, alice.as.URL)
	if err != nil || did != aliceDID {
		t.Fatalf("%q, %v", did, err)
	}
	if len(alice.tokens) != 1 || alice.tokens[0].Get("code") != "the-code" || alice.tokens[0].Get("redirect_uri") != testOrigin+"/oauth/callback" {
		t.Errorf("token request %v", alice.tokens)
	}
	// And we don't keep their tokens: revoked with their server, gone from ours.
	if len(alice.revoked) != 1 || alice.revoked[0] != "refresh-1" {
		t.Errorf("revoked %v, want the refresh token", alice.revoked)
	}
	if n := a.sessions.len(); n != 0 {
		t.Errorf("%d sessions kept", n)
	}
	if n := a.states.len(); n != 0 {
		t.Errorf("%d logins still pending", n)
	}

	// A login can't be finished twice.
	if _, err := a.Finish(ctx, "the-code", state, alice.as.URL); !errors.Is(err, oauth.ErrInvalidState) {
		t.Errorf("replay: %v", err)
	}
	if len(alice.tokens) != 1 {
		t.Error("a replayed login reached the token endpoint")
	}
}

func TestAtmosAcceptsADIDToo(t *testing.T) {
	alice := newWorld(t, "alice.example.test", aliceDID)
	a := newTestAtmos(t, resolverFor(alice), loopback())
	_, state, err := a.Start(context.Background(), aliceDID)
	if err != nil {
		t.Fatal(err)
	}
	if did, err := a.Finish(context.Background(), "c", state, alice.as.URL); err != nil || did != aliceDID {
		t.Errorf("%q, %v", did, err)
	}
}

func TestAtmosRefusesALoginFromTheWrongServer(t *testing.T) {
	alice := newWorld(t, "alice.example.test", aliceDID)
	a := newTestAtmos(t, resolverFor(alice), loopback())
	ctx := context.Background()
	_, state, err := a.Start(ctx, "alice.example.test")
	if err != nil {
		t.Fatal(err)
	}
	// The browser comes back saying another server sent it (a mix-up attack).
	if _, err := a.Finish(ctx, "c", state, "https://evil.example.test"); !errors.Is(err, oauth.ErrIssuerMismatch) {
		t.Errorf("wrong iss: %v", err)
	}
	if len(alice.tokens) != 0 {
		t.Error("the code went to the token endpoint anyway")
	}
	// Nor is a state we never issued any good.
	if _, err := a.Finish(ctx, "c", "made-up-state", alice.as.URL); !errors.Is(err, oauth.ErrInvalidState) {
		t.Errorf("unknown state: %v", err)
	}
	// The refused attempt used the state up: the real one can't be tried again after.
	if _, err := a.Finish(ctx, "c", state, alice.as.URL); err == nil {
		t.Error("a state that was refused once should be gone")
	}
}

// The property everything rests on: a server can only vouch for the accounts that name it. Mallory
// runs her own server and signs herself in there; it answers that the account is Alice's. Alice's
// own server (her DID names it) is another one, so this must fail.
func TestAtmosRefusesAServerThatNamesSomeoneElsesAccount(t *testing.T) {
	alice := newWorld(t, "alice.example.test", aliceDID)
	mallory := newWorld(t, "mallory.example.test", malloryDID)
	mallory.tokenSub = aliceDID
	a := newTestAtmos(t, resolverFor(alice, mallory), loopback())
	ctx := context.Background()

	_, state, err := a.Start(ctx, "mallory.example.test")
	if err != nil {
		t.Fatal(err)
	}
	did, err := a.Finish(ctx, "c", state, mallory.as.URL)
	if !errors.Is(err, oauth.ErrIssuerVerification) || did != "" {
		t.Fatalf("signed in as %q (%v): an authorization server named an account that doesn't use it", did, err)
	}
	if n := a.sessions.len(); n != 0 {
		t.Errorf("%d sessions kept", n)
	}

	// Her own account, through her own server, is fine.
	mallory.mu.Lock()
	mallory.tokenSub = malloryDID
	mallory.mu.Unlock()
	_, state, err = a.Start(ctx, "mallory.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if did, err := a.Finish(ctx, "c", state, mallory.as.URL); err != nil || did != malloryDID {
		t.Errorf("her own: %q, %v", did, err)
	}
}

func TestAtmosFinishesEvenIfTheServerWontRevoke(t *testing.T) {
	alice := newWorld(t, "alice.example.test", aliceDID)
	alice.revokeStatus = 500
	a := newTestAtmos(t, resolverFor(alice), loopback())
	_, state, err := a.Start(context.Background(), "alice.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if did, err := a.Finish(context.Background(), "c", state, alice.as.URL); err != nil || did != aliceDID {
		t.Fatalf("%q, %v", did, err)
	}
	if n := a.sessions.len(); n != 0 {
		t.Errorf("%d sessions kept after a failed revoke: the tokens must be dropped anyway", n)
	}
}

// noDelete is a session store that keeps whatever it is given, to show that finishing a
// login doesn't depend on the library cleaning up after itself.
type noDelete struct{ oauth.SessionStore }

func (noDelete) DeleteSession(context.Context, string, string) error { return nil }

func TestAtmosDropsTheTokensItselfIfTheLibraryDoesNot(t *testing.T) {
	alice := newWorld(t, "alice.example.test", aliceDID)
	a := newTestAtmos(t, resolverFor(alice), loopback())
	a.client.SessionStore = noDelete{a.sessions}
	_, state, err := a.Start(context.Background(), "alice.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if did, err := a.Finish(context.Background(), "c", state, alice.as.URL); err != nil || did != aliceDID {
		t.Fatalf("%q, %v", did, err)
	}
	if n := a.sessions.len(); n != 0 {
		t.Errorf("%d sessions kept: the tokens must not outlive the login", n)
	}
}

func TestAtmosRefusesWhatIsNotAnAccountWithoutAskingAnyone(t *testing.T) {
	alice := newWorld(t, "alice.example.test", aliceDID)
	a := newTestAtmos(t, resolverFor(alice), loopback())
	for _, bad := range []string{"", " ", "not a handle", "alice", "@", "https://alice.example.test", "alice.example.test/x", "did:", "did:plc:", "a b.test", "-x.test", strings.Repeat("a", 300) + ".test"} {
		if _, _, err := a.Start(context.Background(), bad); !errors.Is(err, ErrBadAccount) {
			t.Errorf("%q: %v", bad, err)
		}
	}
	if n := alice.hits.Load(); n != 0 {
		t.Errorf("%d requests were made for things that aren't accounts", n)
	}
}

func TestAtmosAccountThatDoesNotExist(t *testing.T) {
	alice := newWorld(t, "alice.example.test", aliceDID)
	a := newTestAtmos(t, resolverFor(alice), loopback())
	for _, account := range []string{"nobody.example.test", "did:plc:nonexistentnonexistent"} {
		_, _, err := a.Start(context.Background(), account)
		if err == nil || errors.Is(err, ErrBadAccount) {
			t.Errorf("%s: %v, want a lookup failure", account, err)
		}
	}
	if len(alice.par) != 0 {
		t.Error("a login was started for an account that doesn't exist")
	}
}

// Anyone can point their own handle's DNS at Alice's DID. That gets them nothing: the login
// goes to Alice's own server, where only Alice can approve it, and the account we end up with
// is the one that server vouches for, never one that came from what was typed.
func TestAtmosHandleThatPointsAtSomeoneElsesAccountOnlyReachesTheirServer(t *testing.T) {
	alice := newWorld(t, "alice.example.test", aliceDID)
	mallory := newWorld(t, "mallory.example.test", malloryDID)
	res := resolverFor(alice, mallory)
	res.handles["alice-is-me.example.test"] = aliceDID // Mallory's DNS says so; Alice's document doesn't
	a := newTestAtmos(t, res, loopback())

	redirect, state, err := a.Start(context.Background(), "alice-is-me.example.test")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(redirect, alice.as.URL+"/") || len(mallory.par) != 0 {
		t.Fatalf("redirect %q: it must go to Alice's authorization server, not one Mallory chose", redirect)
	}
	// Mallory's server can't stand in for it.
	if _, err := a.Finish(context.Background(), "c", state, mallory.as.URL); !errors.Is(err, oauth.ErrIssuerMismatch) {
		t.Errorf("finishing from Mallory's server: %v", err)
	}
}

func TestAtmosLimitsLoginsInProgress(t *testing.T) {
	alice := newWorld(t, "alice.example.test", aliceDID)
	a := newTestAtmos(t, resolverFor(alice), loopback())
	a.states.max = 2
	for i := range 2 {
		if _, _, err := a.Start(context.Background(), "alice.example.test"); err != nil {
			t.Fatalf("login %d: %v", i, err)
		}
	}
	if _, _, err := a.Start(context.Background(), "alice.example.test"); !errors.Is(err, ErrBusy) {
		t.Errorf("past the limit: %v", err)
	}
}

func TestAtmosDoesNotReachPrivateAddressesUnlessTold(t *testing.T) {
	// With no HTTP client given, the library's own is used, and it won't call servers on
	// private addresses: the fake servers here are on loopback.
	alice := newWorld(t, "alice.example.test", aliceDID)
	a := newTestAtmos(t, resolverFor(alice), nil)
	if _, _, err := a.Start(context.Background(), "alice.example.test"); err == nil {
		t.Error("a login started against a PDS on a private address")
	}
	if n := alice.hits.Load(); n != 0 {
		t.Errorf("the PDS or authorization server was reached %d times", n)
	}
}

func TestNewAtmosChecksItsConfig(t *testing.T) {
	dir := &identity.Directory{Resolver: &fakeResolver{}}
	for name, cfg := range map[string]AtmosConfig{
		"no origin":    {Directory: dir},
		"plain http":   {Origin: "http://feeds.example.test", Directory: dir},
		"a path":       {Origin: testOrigin + "/x", Directory: dir},
		"no directory": {Origin: testOrigin},
	} {
		if _, err := NewAtmos(cfg); err == nil {
			t.Errorf("accepted %s", name)
		}
	}
	a, err := NewAtmos(AtmosConfig{Origin: testOrigin, Directory: dir})
	if err != nil {
		t.Fatal(err)
	}
	m := a.Metadata()
	if m.ClientID != testOrigin+"/oauth/client-metadata.json" || m.Scope != "atproto" || m.TokenEndpointAuthMethod != "none" {
		t.Errorf("%+v", m)
	}
}

// The real wiring end to end: the handler with the real authenticator, a browser (cookies
// carried by hand), and the fake servers.
func TestSigningInThroughTheHandlerAndAtmos(t *testing.T) {
	alice := newWorld(t, "alice.example.test", aliceDID)
	a := newTestAtmos(t, resolverFor(alice), loopback())
	r := newRig(t, func(c *Config) { c.Auth = a; c.Metadata = a.Metadata() })

	// A browser that enforces the cookie rules, as the page's script (asking for JSON) starts the
	// login and the person's own server later sends them back.
	jar := &browserJar{}
	w := r.visit(jar, "POST", "/oauth/login", url.Values{"handle": {"alice.example.test"}}, asJSON)
	var started map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &started); err != nil || w.Code != 200 || !strings.HasPrefix(started["redirect"], alice.as.URL+"/oauth/authorize") {
		t.Fatalf("%d %s", w.Code, w.Body.String())
	}
	state := cookieNamed(w, stateCookieName)
	if state == nil || !jar.has(stateCookieName) {
		t.Fatalf("the browser did not keep the cookie that ties the login to it (refused: %v)", jar.rejected)
	}
	back := r.visit(jar, "GET", "/oauth/callback?code=the-code&state="+url.QueryEscape(state.Value)+"&iss="+url.QueryEscape(alice.as.URL), nil, nil)
	if back.Code != http.StatusSeeOther || location(back) != "/me" {
		t.Fatalf("%d %q", back.Code, location(back))
	}
	if !jar.has(sessionCookieName) {
		t.Fatalf("not signed in (refused: %v)", jar.rejected)
	}
	me := r.visit(jar, "GET", "/api/me", nil, nil)
	if me.Code != 200 || !strings.Contains(me.Body.String(), aliceDID) {
		t.Errorf("/api/me: %d %s", me.Code, me.Body.String())
	}
	if len(alice.revoked) != 1 {
		t.Errorf("revoked %v", alice.revoked)
	}
	jar.mustHaveRefusedNothing(t)
}
