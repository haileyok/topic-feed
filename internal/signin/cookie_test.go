package signin

import (
	"encoding/base64"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"
)

const testDID = "did:plc:ragtjsm2j2vknwkz3zp4oxrd"

var testSecret = []byte("0123456789abcdef0123456789abcdef")

var epoch = time.Date(2026, 10, 1, 12, 0, 0, 0, time.UTC)

type clock struct {
	mu sync.Mutex
	t  time.Time
}

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	c.t = c.t.Add(d)
	c.mu.Unlock()
}

func newSessions(t *testing.T) (*Sessions, *clock) {
	t.Helper()
	s, err := NewSessions(testSecret, 24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	c := &clock{t: epoch}
	s.Now = c.Now
	return s, c
}

func TestSessionRoundTrip(t *testing.T) {
	s, _ := newSessions(t)
	for _, did := range []string{testDID, "did:web:example.com", "did:web:localhost%3A8080"} {
		got, ok := s.Verify(s.Issue(did))
		if !ok || got != did {
			t.Errorf("%s: got %q, %v", did, got, ok)
		}
	}
}

func TestSessionExpires(t *testing.T) {
	s, clk := newSessions(t)
	v := s.Issue(testDID)
	clk.Advance(24*time.Hour - time.Second)
	if _, ok := s.Verify(v); !ok {
		t.Error("refused before it expired")
	}
	clk.Advance(time.Second)
	if _, ok := s.Verify(v); ok {
		t.Error("accepted when it expired")
	}
	clk.Advance(100 * 24 * time.Hour)
	if _, ok := s.Verify(v); ok {
		t.Error("accepted long after it expired")
	}
}

func TestSessionRefusesWhatHasBeenChanged(t *testing.T) {
	s, _ := newSessions(t)
	v := s.Issue(testDID)
	parts := strings.Split(v, ".")
	b64 := func(s string) string { return base64.RawURLEncoding.EncodeToString([]byte(s)) }
	other := "did:plc:aaaaaaaaaaaaaaaaaaaaaaaa"
	farFuture := strconv.FormatInt(epoch.Add(1000*24*time.Hour).Unix(), 10)

	// Somebody else's account with this account's signature, a later expiry, a changed
	// signature, and values that were never ours.
	flipped := []byte(parts[3])
	flipped[0] ^= 1
	forged := map[string]string{
		"another account":       strings.Join([]string{parts[0], b64(other), parts[2], parts[3]}, "."),
		"a later expiry":        strings.Join([]string{parts[0], parts[1], farFuture, parts[3]}, "."),
		"a changed signature":   strings.Join([]string{parts[0], parts[1], parts[2], string(flipped)}, "."),
		"no signature":          strings.Join(parts[:3], ".") + ".",
		"a short signature":     strings.Join([]string{parts[0], parts[1], parts[2], parts[3][:10]}, "."),
		"another version":       strings.Join([]string{"v2", parts[1], parts[2], parts[3]}, "."),
		"an extra part":         v + ".x",
		"a missing part":        strings.Join(parts[:3], "."),
		"empty":                 "",
		"dots":                  "...",
		"text":                  "hello",
		"padding in the expiry": strings.Join([]string{parts[0], parts[1], "+" + parts[2], parts[3]}, "."),
		"a leading zero":        strings.Join([]string{parts[0], parts[1], "0" + parts[2], parts[3]}, "."),
		"the DID, bare":         testDID,
	}
	for name, value := range forged {
		if did, ok := s.Verify(value); ok {
			t.Errorf("accepted %s: %q", name, did)
		}
	}
	if _, ok := s.Verify(v); !ok {
		t.Fatal("the genuine one was refused")
	}
}

func TestSessionFromAnotherSecretIsRefused(t *testing.T) {
	a, _ := newSessions(t)
	b, err := NewSessions([]byte("a different secret, also 32 bytes!"), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	b.Now = a.Now
	if _, ok := b.Verify(a.Issue(testDID)); ok {
		t.Error("a cookie signed with another secret was accepted")
	}
}

func TestSessionRefusesSignedValuesThatAreNotDIDs(t *testing.T) {
	s, _ := newSessions(t)
	// Correctly signed, but not an account: only possible if something issued it, which
	// nothing should, so it is refused rather than passed on.
	for _, notDID := range []string{"", "alice.test", "did:", "did:plc:", "<script>", "did:plc:abc def"} {
		expires := epoch.Add(time.Hour).Unix()
		v := "v1." + base64.RawURLEncoding.EncodeToString([]byte(notDID)) + "." + strconv.FormatInt(expires, 10) + "." +
			base64.RawURLEncoding.EncodeToString(s.sign(notDID, expires))
		if did, ok := s.Verify(v); ok {
			t.Errorf("accepted %q as %q", notDID, did)
		}
	}
}

func TestSessionSignatureIsForSessionsOnly(t *testing.T) {
	// The same secret may sign other things one day: a signature over just the DID and
	// expiry, without the purpose, must not do.
	s, _ := newSessions(t)
	expires := epoch.Add(time.Hour).Unix()
	bare := hmacOf(testSecret, testDID+"\x00"+strconv.FormatInt(expires, 10))
	v := "v1." + base64.RawURLEncoding.EncodeToString([]byte(testDID)) + "." + strconv.FormatInt(expires, 10) + "." +
		base64.RawURLEncoding.EncodeToString(bare)
	if _, ok := s.Verify(v); ok {
		t.Error("a signature made without the session purpose was accepted")
	}
}

func TestNewSessionsChecksItsArguments(t *testing.T) {
	if _, err := NewSessions([]byte("too short"), time.Hour); err == nil {
		t.Error("a short secret was accepted")
	}
	if _, err := NewSessions(make([]byte, MinSecretBytes-1), time.Hour); err == nil {
		t.Error("a secret one byte short was accepted")
	}
	if _, err := NewSessions(make([]byte, MinSecretBytes), time.Hour); err != nil {
		t.Errorf("a secret of exactly the minimum: %v", err)
	}
	for _, ttl := range []time.Duration{0, -time.Hour} {
		if _, err := NewSessions(testSecret, ttl); err == nil {
			t.Errorf("a lifetime of %v was accepted", ttl)
		}
	}
}

func TestSessionsKeepTheirOwnCopyOfTheSecret(t *testing.T) {
	secret := []byte("0123456789abcdef0123456789abcdef")
	s, err := NewSessions(secret, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	v := s.Issue(testDID)
	for i := range secret {
		secret[i] = 'x' // the caller reuses its buffer
	}
	if _, ok := s.Verify(v); !ok {
		t.Error("changing the caller's secret changed what is accepted")
	}
}
