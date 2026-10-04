package feedgen

import (
	"errors"
	"strings"
	"testing"

	"github.com/golang-jwt/jwt/v5"
)

// stubMethod stands in for a signing method that some other library registered over ours.
type stubMethod struct {
	alg    string
	verify error
}

func (m stubMethod) Alg() string                      { return m.alg }
func (m stubMethod) Verify(string, []byte, any) error { return m.verify }
func (m stubMethod) Sign(string, any) ([]byte, error) { return []byte("signature"), nil }

// replaceSigningMethod registers m for its algorithm until the test ends.
func replaceSigningMethod(t *testing.T, m stubMethod) {
	t.Helper()
	original := jwt.GetSigningMethod(m.alg)
	if original == nil {
		t.Fatalf("no signing method for %s to replace", m.alg)
	}
	jwt.RegisterSigningMethod(m.alg, func() jwt.SigningMethod { return m })
	t.Cleanup(func() { jwt.RegisterSigningMethod(m.alg, func() jwt.SigningMethod { return original }) })
}

func TestCheckCredentialsPassesAsTheProgramIsBuilt(t *testing.T) {
	if err := CheckCredentials(); err != nil {
		t.Fatal(err)
	}
}

func TestCheckCredentialsNoticesAReplacedSigningMethod(t *testing.T) {
	for _, alg := range []string{"ES256K", "ES256"} {
		t.Run(alg+" that refuses everything", func(t *testing.T) {
			// What happened: another library's method, expecting its own kind of key.
			replaceSigningMethod(t, stubMethod{alg: alg, verify: errors.New("key must be another type")})
			err := CheckCredentials()
			if err == nil || !strings.Contains(err.Error(), alg) || !strings.Contains(err.Error(), "genuine credential was refused") {
				t.Errorf("err %v", err)
			}
		})
		t.Run(alg+" that accepts everything", func(t *testing.T) {
			// Worse: nothing is refused, so anyone could be any viewer.
			replaceSigningMethod(t, stubMethod{alg: alg})
			err := CheckCredentials()
			if err == nil || !strings.Contains(err.Error(), alg) || !strings.Contains(err.Error(), "wrong key was accepted") {
				t.Errorf("err %v", err)
			}
		})
	}
	// And the methods are back as they were.
	if err := CheckCredentials(); err != nil {
		t.Errorf("after restoring them: %v", err)
	}
}
