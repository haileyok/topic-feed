package feedgen

import (
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jcalabro/atmos/crypto"
)

// tokenWith signs exactly these claims with the viewer's key, so a test decides which claims a
// credential has. (serviceauth.CreateToken always adds iat and jti, which is how a credential
// from a server that leaves them out went untested.)
func (v *testViewer) tokenWith(t *testing.T, claims jwt.MapClaims) string {
	t.Helper()
	var alg string
	switch v.key.(type) {
	case *crypto.K256PrivateKey:
		alg = "ES256K"
	case *crypto.P256PrivateKey:
		alg = "ES256"
	default:
		t.Fatalf("unsupported key type %T", v.key)
	}
	tok, err := jwt.NewWithClaims(jwt.GetSigningMethod(alg), claims).SignedString(v.key)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}

// Servers older than atproto's change that added `iat` to service credentials, and ones that
// have never sent `jti`, sign credentials without them. atproto's own verifier asks only for iss,
// aud and exp (and lxm where a method is named), so those credentials are valid, and a viewer
// whose server sends them must still be recognized: before this was handled every one of them
// was refused and the viewer saw only the welcome post.
func TestViewerCredentialsWithoutIatOrJti(t *testing.T) {
	viewer := newTestViewer(t, "did:plc:viewer")
	stranger := newTestViewer(t, "did:plc:viewer") // same account name, someone else's key
	s := credentialServer(t, testDirectory(viewer.doc()))
	now := time.Now()
	method := interactionsMethod.String()

	// claims is what a PDS signs, then edited.
	claims := func(edit func(jwt.MapClaims)) jwt.MapClaims {
		c := jwt.MapClaims{"iss": viewer.did, "aud": serviceAud, "lxm": method, "exp": now.Add(time.Minute).Unix()}
		if edit != nil {
			edit(c)
		}
		return c
	}
	with := func(k string, v any) func(jwt.MapClaims) { return func(c jwt.MapClaims) { c[k] = v } }
	without := func(k string) func(jwt.MapClaims) { return func(c jwt.MapClaims) { delete(c, k) } }

	for name, tc := range map[string]struct {
		tok  string
		want int
	}{
		"neither iat nor jti":                 {viewer.tokenWith(t, claims(nil)), 200},
		"jti but no iat":                      {viewer.tokenWith(t, claims(with("jti", "abc"))), 200},
		"iat but no jti":                      {viewer.tokenWith(t, claims(with("iat", now.Unix()))), 200},
		"both, as a current server sends":     {viewer.tokenWith(t, claims(func(c jwt.MapClaims) { c["iat"], c["jti"] = now.Unix(), "abc" })), 200},
		"no iat, the service named with a #":  {viewer.tokenWith(t, claims(with("aud", serviceAud+"#bsky_fg"))), 200},
		"no iat, expired a few seconds ago":   {viewer.tokenWith(t, claims(with("exp", now.Add(-10*time.Second).Unix()))), 200},
		"no iat, expired a while ago":         {viewer.tokenWith(t, claims(with("exp", now.Add(-40*time.Second).Unix()))), 401},
		"no iat, good for a day":              {viewer.tokenWith(t, claims(with("exp", now.Add(24*time.Hour).Unix()))), 401},
		"no exp":                              {viewer.tokenWith(t, claims(func(c jwt.MapClaims) { c["iat"] = now.Unix(); delete(c, "exp") })), 401},
		"no iat, for another service":         {viewer.tokenWith(t, claims(with("aud", "did:web:elsewhere.example.com"))), 401},
		"no iat, for another method":          {viewer.tokenWith(t, claims(with("lxm", skeletonMethod.String()))), 401},
		"no iat, for no method":               {viewer.tokenWith(t, claims(without("lxm"))), 401},
		"no iat, signed with someone's key":   {stranger.tokenWith(t, claims(nil)), 401},
		"no iat, from an account not found":   {viewer.tokenWith(t, claims(with("iss", "did:plc:nobody"))), 401},
		"iat a day in the future":             {viewer.tokenWith(t, claims(with("iat", now.Add(24*time.Hour).Unix()))), 401},
		"iat an hour ago (too old to accept)": {viewer.tokenWith(t, claims(with("iat", now.Add(-time.Hour).Unix()))), 401},
	} {
		if got := credentialStatus(s, bearer(tc.tok)); got != tc.want {
			t.Errorf("%s: %d, want %d", name, got, tc.want)
		}
	}
}

// The same holds for the other kind of signing key an account can have.
func TestViewerCredentialsWithoutIatWithAP256Key(t *testing.T) {
	key, err := crypto.GenerateP256()
	if err != nil {
		t.Fatal(err)
	}
	viewer := &testViewer{did: "did:plc:viewer", key: key}
	s := credentialServer(t, testDirectory(viewer.doc()))
	claims := jwt.MapClaims{"iss": viewer.did, "aud": serviceAud, "lxm": interactionsMethod.String(), "exp": time.Now().Add(time.Minute).Unix()}
	if got := credentialStatus(s, bearer(viewer.tokenWith(t, claims))); got != 200 {
		t.Errorf("%d, want 200", got)
	}
	other := &testViewer{did: viewer.did, key: mustK256(t)}
	if got := credentialStatus(s, bearer(other.tokenWith(t, claims))); got != 401 {
		t.Errorf("a K-256 signature for a P-256 account: %d, want 401", got)
	}
}
