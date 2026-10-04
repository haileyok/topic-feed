// Package signin lets a person prove which Bluesky account is theirs by signing in with
// atproto OAuth, and remembers it in a signed cookie.
//
// OAuth is used only to learn the account's DID, which the library verifies (the account's
// own server must vouch for the authorization server that answered). The tokens that come
// with it are revoked at once and never stored: nothing here acts on the person's behalf.
package signin

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// MinSecretBytes is the shortest secret that can sign cookies.
const MinSecretBytes = 32

// cookiePurpose keeps a signature made for a session cookie from being valid for anything else
// signed with the same secret.
const cookiePurpose = "topic-feed/signin/session/v1"

// Sessions issues and checks the cookie that says which account is signed in. The value is
// version.DID.expiry.signature: the signature (HMAC-SHA256) covers the DID and the expiry, so
// neither can be changed, and an expired value is refused even if it is genuine.
type Sessions struct {
	secret []byte
	ttl    time.Duration
	// Now is the clock; nil: time.Now.
	Now func() time.Time
}

// NewSessions makes sessions that last ttl, signed with secret.
func NewSessions(secret []byte, ttl time.Duration) (*Sessions, error) {
	if len(secret) < MinSecretBytes {
		return nil, fmt.Errorf("the session secret must be at least %d bytes", MinSecretBytes)
	}
	if ttl <= 0 {
		return nil, errors.New("the session lifetime must be positive")
	}
	return &Sessions{secret: append([]byte(nil), secret...), ttl: ttl}, nil
}

// TTL is how long a cookie from Issue is good for.
func (s *Sessions) TTL() time.Duration { return s.ttl }

func (s *Sessions) now() time.Time {
	if s.Now != nil {
		return s.Now()
	}
	return time.Now()
}

func (s *Sessions) sign(did string, expires int64) []byte {
	m := hmac.New(sha256.New, s.secret)
	m.Write([]byte(cookiePurpose))
	m.Write([]byte{0})
	m.Write([]byte(did))
	m.Write([]byte{0})
	m.Write([]byte(strconv.FormatInt(expires, 10)))
	return m.Sum(nil)
}

// Issue returns the cookie value for a signed-in account.
func (s *Sessions) Issue(did string) string {
	expires := s.now().Add(s.ttl).Unix()
	return "v1." + base64.RawURLEncoding.EncodeToString([]byte(did)) + "." + strconv.FormatInt(expires, 10) +
		"." + base64.RawURLEncoding.EncodeToString(s.sign(did, expires))
}

// Verify returns the account a cookie value was issued for, or false if the value is not
// one of ours, has been changed, or has expired.
func (s *Sessions) Verify(value string) (string, bool) {
	parts := strings.Split(value, ".")
	if len(parts) != 4 || parts[0] != "v1" {
		return "", false
	}
	didBytes, err := base64.RawURLEncoding.DecodeString(parts[1])
	if err != nil {
		return "", false
	}
	expires, err := strconv.ParseInt(parts[2], 10, 64)
	if err != nil || strconv.FormatInt(expires, 10) != parts[2] {
		return "", false
	}
	mac, err := base64.RawURLEncoding.DecodeString(parts[3])
	if err != nil {
		return "", false
	}
	did := string(didBytes)
	if !hmac.Equal(mac, s.sign(did, expires)) {
		return "", false
	}
	if !s.now().Before(time.Unix(expires, 0)) {
		return "", false
	}
	if _, err := syntax.ParseDID(did); err != nil {
		return "", false
	}
	return did, true
}
