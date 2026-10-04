package feedgen

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"github.com/jcalabro/atmos"
	atmosidentity "github.com/jcalabro/atmos/identity"
)

const (
	// credentialMaxAge is how old a credential that says when it was issued may be, and how far
	// ahead of now one may expire: they are minutes long, so anything beyond is not one.
	credentialMaxAge = 5 * time.Minute
	// credentialLeeway is the clock difference put up with between us and the server that
	// signed the credential.
	credentialLeeway = 30 * time.Second
)

// credentialClaims are the claims of a service credential that are read, besides the registered
// ones (iss, aud, exp, iat, ...).
type credentialClaims struct {
	jwt.RegisteredClaims
	LexMethod string `json:"lxm,omitempty"`
}

// verifyCredential checks a viewer's service credential and returns the account that signed it.
// token must be signed with a key in the account's DID document, be meant for audience, name
// method as its lexicon method (lxm), and expire within a few minutes.
//
// It asks what atproto's own verifier asks, and no more: iss, aud and exp are required, but iat
// and jti are not. Servers older than the change that added iat don't send it, and some never
// send jti; their credentials are valid, and a library that insists on both (as
// serviceauth.VerifyToken does) leaves every viewer of such a server looking anonymous. When
// there is no iat the credential's age can't be checked, so its lifetime is bounded by how far
// ahead its exp may be instead.
//
// The signing methods come from atmos's serviceauth package, which registers them in the JWT
// library when it is linked in (see CheckCredentials).
func verifyCredential(ctx context.Context, token, audience string, method atmos.NSID, dir *atmosidentity.Directory) (atmos.DID, error) {
	if audience == "" || dir == nil {
		return "", errors.New("credential: an audience and an identity directory are required")
	}
	return verifyCredentialOnce(ctx, token, audience, method, dir, false)
}

func verifyCredentialOnce(ctx context.Context, token, audience string, method atmos.NSID, dir *atmosidentity.Directory, retried bool) (atmos.DID, error) {
	var c credentialClaims
	_, err := jwt.ParseWithClaims(token, &c, func(t *jwt.Token) (any, error) {
		// Only the plain JWT type: an OAuth access token (at+jwt) or a DPoP proof (dpop+jwt)
		// signed by the same key must not pass for a credential. A name is compared without
		// regard to case, and one without a '/' stands for "application/<name>" (RFC 7515 4.1.9).
		if typ, present := t.Header["typ"]; present {
			s, ok := typ.(string)
			if !ok {
				return nil, errors.New("credential: the token type must be a string")
			}
			if !strings.EqualFold(s, "JWT") && !strings.EqualFold(s, "application/JWT") {
				return nil, fmt.Errorf("credential: unsupported token type %q", s)
			}
		}
		did, fragment, err := splitIssuer(c.Issuer)
		if err != nil {
			return nil, fmt.Errorf("credential: invalid issuer: %w", err)
		}
		id, err := dir.LookupDID(ctx, did)
		if err != nil {
			return nil, fmt.Errorf("credential: resolving the issuer: %w", err)
		}
		// An issuer may name a key other than the atproto one with a fragment
		// (did:plc:xxx#atproto_labeler).
		pub, err := id.PublicKeyForFragment(fragment)
		if err != nil {
			return nil, fmt.Errorf("credential: the issuer has no such signing key: %w", err)
		}
		return pub, nil
	},
		jwt.WithValidMethods([]string{"ES256", "ES256K"}),
		jwt.WithAudience(audience),
		jwt.WithLeeway(credentialLeeway),
		jwt.WithExpirationRequired(),
		jwt.WithIssuedAt(), // when there is an iat, it must not be in the future
	)
	if err != nil {
		// A signature that doesn't verify may be a key the account has since replaced, one we
		// have cached: look the account up again, once.
		if errors.Is(err, jwt.ErrTokenSignatureInvalid) && !retried {
			if did, _, perr := splitIssuer(c.Issuer); perr == nil {
				dir.Purge(ctx, did)
				return verifyCredentialOnce(ctx, token, audience, method, dir, true)
			}
		}
		return "", fmt.Errorf("credential: %w", err)
	}

	if iat := c.IssuedAt; iat != nil && time.Since(iat.Time) > credentialMaxAge+credentialLeeway {
		return "", errors.New("credential: too old")
	}
	if c.ExpiresAt == nil {
		return "", errors.New("credential: no expiry")
	}
	if time.Until(c.ExpiresAt.Time) > credentialMaxAge+credentialLeeway {
		return "", errors.New("credential: expires too far ahead")
	}
	if method != "" {
		if c.LexMethod == "" {
			return "", errors.New("credential: no lexicon method (lxm)")
		}
		if atmos.NSID(c.LexMethod) != method {
			return "", fmt.Errorf("credential: made for %q, not %q", c.LexMethod, method)
		}
	}
	did, _, err := splitIssuer(c.Issuer)
	if err != nil {
		return "", fmt.Errorf("credential: invalid issuer: %w", err)
	}
	return did, nil
}

// splitIssuer splits an issuer claim into its account and the optional name of the key in
// the account's DID document that signed it ("did:plc:xxx#atproto_labeler"), without the '#'.
func splitIssuer(iss string) (atmos.DID, string, error) {
	bare, fragment, hasFragment := strings.Cut(iss, "#")
	did, err := atmos.ParseDID(bare)
	if err != nil {
		return "", "", err
	}
	if hasFragment && (fragment == "" || strings.Contains(fragment, "#")) {
		return "", "", errors.New("invalid key name after the #")
	}
	return did, fragment, nil
}
