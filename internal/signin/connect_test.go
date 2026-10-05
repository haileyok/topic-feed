package signin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/jcalabro/atmos/crypto"
	"github.com/jcalabro/atmos/identity"
	"github.com/jcalabro/atmos/oauth"
)

// memLogins is a LoginStore in memory.
type memLogins struct {
	mu   sync.Mutex
	rows map[string][]byte
}

func (m *memLogins) LoadLogin(_ context.Context, did string) ([]byte, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.rows[did], nil
}

func (m *memLogins) SaveLogin(_ context.Context, did string, sealed []byte) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.rows == nil {
		m.rows = map[string][]byte{}
	}
	m.rows[did] = sealed
	return nil
}

func (m *memLogins) DeleteLogin(_ context.Context, did string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.rows, did)
	return nil
}

const testAud = "did:web:source.test#bsky_fg"

func testConnector(t *testing.T, store LoginStore, client *http.Client) *Connector {
	t.Helper()
	c, err := NewConnector(ConnectConfig{
		Origin: "https://feeds.example.test", Secret: []byte(strings.Repeat("s", 32)),
		Directory: &identity.Directory{}, Audiences: []string{testAud}, Store: store, HTTPClient: client,
		Log: slog.New(slog.NewTextHandler(io.Discard, nil)),
	})
	if err != nil {
		t.Fatal(err)
	}
	return c
}

func TestConnectScopeNamesEachAudienceBothWays(t *testing.T) {
	got := ConnectScope([]string{"did:web:a.test#bsky_fg"})
	want := "atproto rpc?lxm=app.bsky.feed.getFeedSkeleton&lxm=app.bsky.feed.sendInteractions&aud=did%3Aweb%3Aa.test%23bsky_fg" +
		" rpc?lxm=app.bsky.feed.getFeedSkeleton&lxm=app.bsky.feed.sendInteractions&aud=did%3Aweb%3Aa.test"
	if got != want {
		t.Errorf("scope\n got %s\nwant %s", got, want)
	}
}

func TestConnectorIsAConfidentialClient(t *testing.T) {
	c := testConnector(t, &memLogins{}, nil)
	m := c.Metadata()
	if m.TokenEndpointAuthMethod != "private_key_jwt" || m.JWKS == nil || len(m.JWKS.Keys) != 1 ||
		m.ClientID != "https://feeds.example.test/oauth/connect-metadata.json" ||
		m.RedirectURIs[0] != "https://feeds.example.test/oauth/connect-callback" || !strings.Contains(m.Scope, "source.test") {
		t.Errorf("metadata: %+v", m)
	}
	// The same secret makes the same key: a restart doesn't change who the service is.
	if again := testConnector(t, &memLogins{}, nil).Metadata(); again.JWKS.Keys[0] != m.JWKS.Keys[0] {
		t.Error("the key changed for the same secret")
	}
	if _, err := NewConnector(ConnectConfig{Origin: "https://x.test", Secret: []byte("short"), Directory: &identity.Directory{},
		Audiences: []string{testAud}, Store: &memLogins{}}); err == nil {
		t.Error("accepted a short secret")
	}
}

func testSession(t *testing.T, did, pds string) *oauth.Session {
	t.Helper()
	key, err := crypto.GenerateP256()
	if err != nil {
		t.Fatal(err)
	}
	return &oauth.Session{SessionID: "sess-1", DPoPKey: key, TokenSet: oauth.TokenSet{
		Sub: did, Aud: pds, AccessToken: "access", ExpiresAt: time.Now().Add(time.Hour), RefreshToken: "refresh"}}
}

func TestSignInsAreSealedToTheirViewer(t *testing.T) {
	store := &memLogins{}
	c := testConnector(t, store, nil)
	sess := testSession(t, "did:plc:alice", "https://pds.test")
	if err := c.sessions.SetSession(context.Background(), sess); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(store.rows["did:plc:alice"]), "refresh") {
		t.Error("the stored sign-in holds its refresh token in the clear")
	}
	back, err := c.sessions.GetSession(context.Background(), "did:plc:alice", "sess-1")
	if err != nil || back.TokenSet.RefreshToken != "refresh" {
		t.Fatalf("back: %+v %v", back, err)
	}
	// Moved to another viewer, it doesn't open.
	store.rows["did:plc:mallory"] = store.rows["did:plc:alice"]
	if _, err := c.sessions.GetSession(context.Background(), "did:plc:mallory", "sess-1"); !errors.Is(err, oauth.ErrNoSession) {
		t.Errorf("another viewer's sign-in opened: %v", err)
	}
	if ok, _ := c.Connected(context.Background(), "did:plc:mallory"); ok {
		t.Error("a sign-in moved to another viewer counts as theirs")
	}
}

// fakePDS answers getServiceAuth. Like some servers, it refuses an audience with a #service.
type fakePDS struct {
	mu     sync.Mutex
	calls  []string
	status int
}

func (p *fakePDS) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	p.mu.Lock()
	defer p.mu.Unlock()
	q := r.URL.Query()
	p.calls = append(p.calls, q.Get("aud"))
	w.Header().Set("Content-Type", "application/json")
	switch {
	case !strings.HasPrefix(r.Header.Get("Authorization"), "DPoP access") || r.Header.Get("DPoP") == "":
		w.WriteHeader(http.StatusUnauthorized)
		w.Write([]byte(`{"error":"AuthRequired"}`))
	case p.status != 0:
		w.WriteHeader(p.status)
		w.Write([]byte(`{"error":"InvalidToken"}`))
	case strings.Contains(q.Get("aud"), "#"):
		w.WriteHeader(http.StatusBadRequest)
		w.Write([]byte(`{"error":"InvalidRequest"}`))
	default:
		json.NewEncoder(w).Encode(map[string]string{"token": "tok:" + q.Get("aud") + ":" + q.Get("lxm")})
	}
}

func TestTokenAsksTheViewersServerAndRemembers(t *testing.T) {
	pds := &fakePDS{}
	ts := httptest.NewServer(pds)
	defer ts.Close()
	store := &memLogins{}
	c := testConnector(t, store, ts.Client())
	ctx := context.Background()
	if _, err := c.Token(ctx, "did:plc:alice", testAud, FeedSkeletonMethod); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("no sign-in: %v", err)
	}
	if err := c.sessions.SetSession(ctx, testSession(t, "did:plc:alice", ts.URL)); err != nil {
		t.Fatal(err)
	}
	tok, err := c.Token(ctx, "did:plc:alice", testAud, FeedSkeletonMethod)
	if err != nil || tok != "tok:did:web:source.test:"+FeedSkeletonMethod {
		t.Fatalf("token %q %v", tok, err)
	}
	if len(pds.calls) != 2 || pds.calls[0] != testAud || pds.calls[1] != "did:web:source.test" {
		t.Errorf("asked for %v: want the #service form, then the bare DID", pds.calls)
	}
	if again, _ := c.Token(ctx, "did:plc:alice", testAud, FeedSkeletonMethod); again != tok || len(pds.calls) != 2 {
		t.Errorf("not remembered: %d calls", len(pds.calls))
	}
	if other, _ := c.Token(ctx, "did:plc:alice", testAud, InteractionsMethod); !strings.HasSuffix(other, InteractionsMethod) {
		t.Errorf("a token for interactions: %q", other)
	}
}

func TestTokenForgetsASignInThatNoLongerWorks(t *testing.T) {
	pds := &fakePDS{status: http.StatusUnauthorized}
	ts := httptest.NewServer(pds)
	defer ts.Close()
	store := &memLogins{}
	c := testConnector(t, store, ts.Client())
	ctx := context.Background()
	if err := c.sessions.SetSession(ctx, testSession(t, "did:plc:alice", ts.URL)); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Token(ctx, "did:plc:alice", testAud, FeedSkeletonMethod); !errors.Is(err, ErrNotConnected) {
		t.Fatalf("a revoked sign-in: %v", err)
	}
	if ok, _ := c.Connected(ctx, "did:plc:alice"); ok {
		t.Error("a sign-in that no longer works is still kept")
	}
}

func TestTokenPassesOnAServerThatIsDown(t *testing.T) {
	pds := &fakePDS{status: http.StatusBadGateway}
	ts := httptest.NewServer(pds)
	defer ts.Close()
	store := &memLogins{}
	c := testConnector(t, store, ts.Client())
	ctx := context.Background()
	c.sessions.SetSession(ctx, testSession(t, "did:plc:alice", ts.URL))
	if _, err := c.Token(ctx, "did:plc:alice", testAud, FeedSkeletonMethod); err == nil || errors.Is(err, ErrNotConnected) {
		t.Fatalf("a server that is down: %v", err)
	}
	if ok, _ := c.Connected(ctx, "did:plc:alice"); !ok {
		t.Error("a server being down forgot the sign-in")
	}
}
