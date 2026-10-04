package feedgen

import (
	"context"
	"fmt"
	"testing"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/jcalabro/atmos"
	"github.com/jcalabro/atmos/crypto"
	atmosidentity "github.com/jcalabro/atmos/identity"
	"github.com/jcalabro/atmos/serviceauth"
)

// didDocs finds the accounts it was given and nothing else, so tests never touch the network.
type didDocs struct {
	docs map[string]*atmosidentity.DIDDocument
}

func (d didDocs) ResolveDID(_ context.Context, did atmos.DID) (*atmosidentity.DIDDocument, error) {
	if doc, ok := d.docs[string(did)]; ok {
		return doc, nil
	}
	return nil, fmt.Errorf("no such DID %s", did)
}

func (didDocs) ResolveHandle(_ context.Context, h atmos.Handle) (atmos.DID, error) {
	return "", fmt.Errorf("no such handle %s", h)
}

// testDirectory is a directory that knows these accounts. Like the one the service runs with,
// it doesn't verify handles.
func testDirectory(docs ...*atmosidentity.DIDDocument) *atmosidentity.Directory {
	m := map[string]*atmosidentity.DIDDocument{}
	for _, d := range docs {
		m[d.ID] = d
	}
	return &atmosidentity.Directory{Resolver: didDocs{m}, SkipHandleVerification: true}
}

// testViewer is an account with a signing key the server can find, and a way to make
// credentials from it, as the Bluesky app does when it asks a feed for a viewer.
type testViewer struct {
	did string
	key crypto.PrivateKey
}

func newTestViewer(t *testing.T, did string) *testViewer {
	t.Helper()
	key, err := crypto.GenerateK256()
	if err != nil {
		t.Fatal(err)
	}
	return &testViewer{did: did, key: key}
}

func (v *testViewer) doc() *atmosidentity.DIDDocument {
	return &atmosidentity.DIDDocument{ID: v.did, AlsoKnownAs: []string{"at://viewer.test"},
		VerificationMethod: []atmosidentity.VerificationMethod{{ID: v.did + "#atproto", Type: "Multikey", Controller: v.did,
			PublicKeyMultibase: v.key.PublicKey().Multibase()}}}
}

// credential is a signed credential for aud and method, good for a minute.
func (v *testViewer) credential(t *testing.T, aud string, method syntax.NSID) string {
	t.Helper()
	return v.credentialExpiring(t, aud, method, time.Now().Add(time.Minute))
}

func (v *testViewer) credentialExpiring(t *testing.T, aud string, method syntax.NSID, exp time.Time) string {
	t.Helper()
	tok, err := serviceauth.CreateToken(serviceauth.TokenParams{
		Issuer: atmos.DID(v.did), Audience: aud, Exp: exp, LexMethod: atmos.NSID(method.String()),
	}, v.key)
	if err != nil {
		t.Fatal(err)
	}
	return tok
}
