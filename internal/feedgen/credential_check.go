package feedgen

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/jcalabro/atmos"
	"github.com/jcalabro/atmos/crypto"
	atmosidentity "github.com/jcalabro/atmos/identity"
	"github.com/jcalabro/atmos/serviceauth"
)

// CheckCredentials makes sure viewers' credentials can be checked in this program, and fails
// if not. Run it at startup.
//
// Every library that handles these JWTs registers its own ES256 and ES256K signing methods in
// one table, shared by everything in the program. Two libraries that both do is a conflict
// nobody sees coming: whichever registers last replaces the other's, and checks done through
// the other start failing (or, worse, passing) without any error at startup. Here that would
// mean every viewer treated as anonymous. So this signs a credential with each kind of key
// and checks it the way the server does, and checks that a credential signed with some other
// key is refused.
func CheckCredentials() error {
	for _, kind := range []struct {
		name string
		new  func() (crypto.PrivateKey, error)
	}{
		{"ES256K", func() (crypto.PrivateKey, error) { return crypto.GenerateK256() }},
		{"ES256", func() (crypto.PrivateKey, error) { return crypto.GenerateP256() }},
	} {
		if err := checkCredentialsOf(kind.name, kind.new); err != nil {
			return fmt.Errorf("viewers' %s credentials can't be checked (is another library replacing the JWT signing methods?): %w", kind.name, err)
		}
	}
	return nil
}

// selfCheckDID and selfCheckAudience name an account and a service that exist only here.
const (
	selfCheckDID      = "did:plc:selfcheckselfcheckselfche"
	selfCheckAudience = "did:web:selfcheck.invalid"
)

func checkCredentialsOf(name string, newKey func() (crypto.PrivateKey, error)) error {
	key, err := newKey()
	if err != nil {
		return err
	}
	other, err := newKey()
	if err != nil {
		return err
	}
	dir := &atmosidentity.Directory{Resolver: staticResolver{&atmosidentity.DIDDocument{
		ID: selfCheckDID,
		VerificationMethod: []atmosidentity.VerificationMethod{{ID: selfCheckDID + "#atproto", Type: "Multikey",
			Controller: selfCheckDID, PublicKeyMultibase: key.PublicKey().Multibase()}},
	}}, SkipHandleVerification: true}

	// The credential is made with atmos's own signer and read with verifyCredential, the check
	// the server runs on viewers' credentials.
	verify := func(signedWith crypto.PrivateKey) (atmos.DID, error) {
		tok, err := serviceauth.CreateToken(serviceauth.TokenParams{
			Issuer: selfCheckDID, Audience: selfCheckAudience, Exp: time.Now().Add(time.Minute), LexMethod: "app.bsky.feed.getFeedSkeleton",
		}, signedWith)
		if err != nil {
			return "", fmt.Errorf("signing: %w", err)
		}
		return verifyCredential(context.Background(), tok, selfCheckAudience, "app.bsky.feed.getFeedSkeleton", dir)
	}
	issuer, err := verify(key)
	if err != nil {
		return fmt.Errorf("a genuine credential was refused: %w", err)
	}
	if issuer != selfCheckDID {
		return fmt.Errorf("a genuine credential was read as coming from %q", issuer)
	}
	if _, err := verify(other); err == nil {
		return errors.New("a credential signed with the wrong key was accepted")
	}
	return nil
}

// staticResolver knows one account.
type staticResolver struct{ doc *atmosidentity.DIDDocument }

func (r staticResolver) ResolveDID(_ context.Context, did atmos.DID) (*atmosidentity.DIDDocument, error) {
	if string(did) == r.doc.ID {
		return r.doc, nil
	}
	return nil, fmt.Errorf("no such DID %s", did)
}

func (staticResolver) ResolveHandle(context.Context, atmos.Handle) (atmos.DID, error) {
	return "", errors.New("handles aren't resolved here")
}
