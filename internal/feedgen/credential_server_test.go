package feedgen

import (
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jcalabro/atmos"
	"github.com/jcalabro/atmos/crypto"
	atmosidentity "github.com/jcalabro/atmos/identity"
	"github.com/jcalabro/atmos/serviceauth"
)

const (
	serviceAud    = "did:web:feeds.example.com"
	interactionOK = `{"interactions":[{"item":"at://did:plc:a/app.bsky.feed.post/2","event":"app.bsky.feed.defs#requestLess","reqId":"nfl-def"}]}`
)

// credentialStatus is the answer to sendInteractions with this Authorization header: 200 for
// a viewer the server accepts, 401 for one it doesn't.
func credentialStatus(s *Server, authorization string) int {
	req := httptest.NewRequest(http.MethodPost, "/xrpc/app.bsky.feed.sendInteractions", strings.NewReader(interactionOK))
	req.Header.Set("Content-Type", "application/json")
	if authorization != "" {
		req.Header.Set("Authorization", authorization)
	}
	rec := httptest.NewRecorder()
	s.Handler().ServeHTTP(rec, req)
	return rec.Code
}

func bearer(token string) string { return "Bearer " + token }

// credentialServer is a server that accepts interactions, which is where a credential's
// outcome shows.
func credentialServer(t *testing.T, dir *atmosidentity.Directory) *Server {
	t.Helper()
	s := testServerWith(t, dir)
	s.Interactions = &fakeSink{}
	return s
}

func (v *testViewer) token(t *testing.T, p serviceauth.TokenParams) string {
	t.Helper()
	tok, err := serviceauth.CreateToken(p, v.key)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

func TestViewerCredentialsAreChecked(t *testing.T) {
	viewer := newTestViewer(t, "did:plc:viewer")
	stranger := newTestViewer(t, "did:plc:viewer") // same account name, someone else's key
	s := credentialServer(t, testDirectory(viewer.doc()))
	method := atmos.NSID(interactionsMethod.String())
	params := func(aud string, exp time.Time, lxm atmos.NSID) serviceauth.TokenParams {
		return serviceauth.TokenParams{Issuer: "did:plc:viewer", Audience: aud, Exp: exp, LexMethod: lxm}
	}
	soon := time.Now().Add(time.Minute)

	for name, tc := range map[string]struct {
		header string
		want   int
	}{
		"a good credential":                      {bearer(viewer.token(t, params(serviceAud, soon, method))), 200},
		"the service named with its fragment":    {bearer(viewer.token(t, params(serviceAud+"#bsky_fg", soon, method))), 200},
		"expired a few seconds ago (clock skew)": {bearer(viewer.token(t, params(serviceAud, time.Now().Add(-10*time.Second), method))), 200},
		"expired a while ago":                    {bearer(viewer.token(t, params(serviceAud, time.Now().Add(-40*time.Second), method))), 401},
		"expired long ago":                       {bearer(viewer.token(t, params(serviceAud, time.Now().Add(-48*time.Hour), method))), 401},
		"signed with someone else's key":         {bearer(stranger.token(t, params(serviceAud, soon, method))), 401},
		"for another service":                    {bearer(viewer.token(t, params("did:web:elsewhere.example.com", soon, method))), 401},
		"for another fragment":                   {bearer(viewer.token(t, params(serviceAud+"#other", soon, method))), 401},
		"for another method":                     {bearer(viewer.token(t, params(serviceAud, soon, atmos.NSID(skeletonMethod.String())))), 401},
		"for no method at all":                   {bearer(viewer.token(t, params(serviceAud, soon, ""))), 401},
		"from an account that can't be found": {bearer((&testViewer{did: "did:plc:nobody", key: viewer.key}).token(t,
			serviceauth.TokenParams{Issuer: "did:plc:nobody", Audience: serviceAud, Exp: soon, LexMethod: method})), 401},
		"no credential":          {"", 401},
		"an empty bearer":        {"Bearer ", 401},
		"another kind of scheme": {"Basic " + base64.StdEncoding.EncodeToString([]byte("viewer:pw")), 401},
		"not a token":            {"Bearer not.a.jwt", 401},
		"a lowercase scheme":     {strings.ToLower(bearer(viewer.token(t, params(serviceAud, soon, method)))), 401},
	} {
		if got := credentialStatus(s, tc.header); got != tc.want {
			t.Errorf("%s: %d, want %d", name, got, tc.want)
		}
	}
}

// Tokens that don't depend on a real signature must never be accepted.
func TestForgedViewerCredentialsAreRefused(t *testing.T) {
	viewer := newTestViewer(t, "did:plc:viewer")
	s := credentialServer(t, testDirectory(viewer.doc()))
	claims := jwt.MapClaims{"iss": "did:plc:viewer", "aud": serviceAud, "lxm": interactionsMethod.String(),
		"exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix(), "jti": "x"}

	b64 := func(v any) string {
		b, _ := json.Marshal(v)
		return base64.RawURLEncoding.EncodeToString(b)
	}
	unsigned := b64(map[string]string{"alg": "none", "typ": "JWT"}) + "." + b64(claims) + "."
	hs256, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte("not the key"))
	if err != nil {
		t.Fatal(err)
	}
	// The classic confusion attack: sign with HMAC, using the public key as the secret.
	hsWithPublicKey, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString(viewer.key.PublicKey().Bytes())
	if err != nil {
		t.Fatal(err)
	}
	good := viewer.credential(t, serviceAud, interactionsMethod)
	parts := strings.Split(good, ".")
	tampered := parts[0] + "." + b64(jwt.MapClaims{"iss": "did:plc:victim", "aud": serviceAud, "lxm": interactionsMethod.String(),
		"exp": time.Now().Add(time.Minute).Unix(), "iat": time.Now().Unix()}) + "." + parts[2]
	badTyp := b64(map[string]string{"alg": "ES256K", "typ": "at+jwt"}) + "." + parts[1] + "." + parts[2]

	if got := credentialStatus(s, bearer(good)); got != 200 {
		t.Fatalf("the genuine credential: %d", got)
	}
	for name, tok := range map[string]string{
		"alg none":                        unsigned,
		"HS256 with a made-up secret":     hs256,
		"HS256 keyed with the public key": hsWithPublicKey,
		"claims changed after signing":    tampered,
		"the wrong token type":            badTyp,
		"the signature cut off":           parts[0] + "." + parts[1] + ".",
		"a truncated signature":           good[:len(good)-6],
	} {
		if got := credentialStatus(s, bearer(tok)); got != 401 {
			t.Errorf("%s: %d, want 401", name, got)
		}
	}
}

func TestViewerCredentialsSurviveAKeyRotation(t *testing.T) {
	old := newTestViewer(t, "did:plc:viewer")
	docs := map[string]*atmosidentity.DIDDocument{old.did: old.doc()}
	// As the service runs: identities cached, handles not verified.
	dir := &atmosidentity.Directory{Resolver: didDocs{docs}, Cache: atmosidentity.NewLRUCache(100, time.Hour), SkipHandleVerification: true}
	s := credentialServer(t, dir)

	if got := credentialStatus(s, bearer(old.credential(t, serviceAud, interactionsMethod))); got != 200 {
		t.Fatalf("before the rotation: %d", got)
	}
	// The viewer rotates their signing key. Their new credentials don't verify against the
	// key we have cached, so the server looks the account up again, and accepts them.
	rotated := &testViewer{did: old.did, key: mustK256(t)}
	docs[old.did] = rotated.doc()
	if got := credentialStatus(s, bearer(rotated.credential(t, serviceAud, interactionsMethod))); got != 200 {
		t.Errorf("a credential from the new key: %d, want it accepted after looking the account up again", got)
	}
	// And the old key no longer works.
	if got := credentialStatus(s, bearer(old.credential(t, serviceAud, interactionsMethod))); got != 401 {
		t.Errorf("a credential from the old key: %d, want 401", got)
	}
}

func mustK256(t *testing.T) crypto.PrivateKey {
	t.Helper()
	k, err := crypto.GenerateK256()
	if err != nil {
		t.Fatal(err)
	}
	return k
}

// A viewer whose key is a P-256 key (the other kind accounts can have) works too.
func TestViewerCredentialsWithAP256Key(t *testing.T) {
	key, err := crypto.GenerateP256()
	if err != nil {
		t.Fatal(err)
	}
	viewer := &testViewer{did: "did:plc:viewer", key: key}
	s := credentialServer(t, testDirectory(viewer.doc()))
	if got := credentialStatus(s, bearer(viewer.credential(t, serviceAud, interactionsMethod))); got != 200 {
		t.Errorf("%d", got)
	}
	other := &testViewer{did: viewer.did, key: mustK256(t)}
	if got := credentialStatus(s, bearer(other.credential(t, serviceAud, interactionsMethod))); got != 401 {
		t.Errorf("a K-256 signature for a P-256 account: %d", got)
	}
}
