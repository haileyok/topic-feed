package signin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"testing"
	"time"

	"crypto/hmac"
	"crypto/sha256"

	"github.com/jcalabro/atmos/oauth"
)

func hmacOf(secret []byte, msg string) []byte {
	m := hmac.New(sha256.New, secret)
	m.Write([]byte(msg))
	return m.Sum(nil)
}

func TestStateStoreExpires(t *testing.T) {
	clk := &clock{t: epoch}
	s := newStateStore(10*time.Minute, 100, clk.Now)
	ctx := context.Background()
	st := &oauth.AuthState{Issuer: "https://as.test"}
	if err := s.SetState(ctx, "a", st); err != nil {
		t.Fatal(err)
	}
	clk.Advance(10*time.Minute - 500*time.Millisecond)
	if got, err := s.GetState(ctx, "a"); err != nil || got != st {
		t.Fatalf("before expiry: %v, %v", got, err)
	}
	// Expired entries are swept at most once a second, and one has just been: the entry is
	// still held, and Get must refuse it by its own expiry.
	clk.Advance(500 * time.Millisecond)
	if _, err := s.GetState(ctx, "a"); !errors.Is(err, oauth.ErrInvalidState) {
		t.Errorf("at expiry: %v", err)
	}
	if _, err := s.GetState(ctx, "never stored"); !errors.Is(err, oauth.ErrInvalidState) {
		t.Errorf("unknown: %v", err)
	}
}

func TestStateStoreDelete(t *testing.T) {
	s := newStateStore(time.Minute, 10, (&clock{t: epoch}).Now)
	ctx := context.Background()
	_ = s.SetState(ctx, "a", &oauth.AuthState{})
	if err := s.DeleteState(ctx, "a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.GetState(ctx, "a"); !errors.Is(err, oauth.ErrInvalidState) {
		t.Errorf("after delete: %v", err)
	}
	if err := s.DeleteState(ctx, "never stored"); err != nil {
		t.Errorf("deleting what isn't there: %v", err)
	}
}

func TestStateStoreHasALimitAndMakesRoomByExpiry(t *testing.T) {
	clk := &clock{t: epoch}
	s := newStateStore(10*time.Minute, 3, clk.Now)
	ctx := context.Background()
	for i := range 3 {
		if err := s.SetState(ctx, fmt.Sprint("s", i), &oauth.AuthState{}); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.SetState(ctx, "one too many", &oauth.AuthState{}); !errors.Is(err, ErrBusy) {
		t.Errorf("past the limit: %v", err)
	}
	if err := s.SetState(ctx, "s1", &oauth.AuthState{Issuer: "replaced"}); err != nil {
		t.Errorf("replacing one that is there at the limit: %v", err)
	}
	// A login that was never finished leaves room once it has expired, even when expired
	// entries were last swept a moment ago: being full forces a sweep.
	clk.Advance(10*time.Minute - 100*time.Millisecond)
	if err := s.SetState(ctx, "just before", &oauth.AuthState{}); !errors.Is(err, ErrBusy) {
		t.Fatalf("nothing has expired yet: %v", err)
	}
	clk.Advance(200 * time.Millisecond)
	if err := s.SetState(ctx, "after expiry", &oauth.AuthState{}); err != nil {
		t.Errorf("expired logins should make room: %v", err)
	}
	if n := s.len(); n != 1 {
		t.Errorf("%d entries held, want only the new one", n)
	}
}

func TestStateStoreUnderConcurrentUse(t *testing.T) {
	s := newStateStore(time.Minute, 50, time.Now)
	ctx := context.Background()
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				key := fmt.Sprint(g, "-", i%40)
				_ = s.SetState(ctx, key, &oauth.AuthState{})
				_, _ = s.GetState(ctx, key)
				_ = s.DeleteState(ctx, key)
			}
		}()
	}
	wg.Wait()
	if n := s.len(); n != 0 {
		t.Errorf("%d entries left", n)
	}
}

func TestSessionStore(t *testing.T) {
	s := newSessionStore()
	ctx := context.Background()
	sess := &oauth.Session{SessionID: "id", TokenSet: oauth.TokenSet{Sub: testDID}}
	if _, err := s.GetSession(ctx, testDID, "id"); !errors.Is(err, oauth.ErrNoSession) {
		t.Errorf("before it is set: %v", err)
	}
	if err := s.SetSession(ctx, sess); err != nil {
		t.Fatal(err)
	}
	if got, err := s.GetSession(ctx, testDID, "id"); err != nil || got != sess {
		t.Errorf("%v, %v", got, err)
	}
	if _, err := s.GetSession(ctx, "did:plc:other", "id"); !errors.Is(err, oauth.ErrNoSession) {
		t.Errorf("another account's: %v", err)
	}
	if _, err := s.GetSession(ctx, testDID, "other"); !errors.Is(err, oauth.ErrNoSession) {
		t.Errorf("another session's: %v", err)
	}
	for _, bad := range []*oauth.Session{nil, {SessionID: "id"}, {TokenSet: oauth.TokenSet{Sub: testDID}}} {
		if err := s.SetSession(ctx, bad); err == nil {
			t.Errorf("accepted %+v", bad)
		}
	}
	if err := s.DeleteSession(ctx, testDID, "id"); err != nil || s.len() != 0 {
		t.Errorf("delete: %v, %d left", err, s.len())
	}
}

func TestSessionStoreUnderConcurrentUse(t *testing.T) {
	s := newSessionStore()
	ctx := context.Background()
	var wg sync.WaitGroup
	for g := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 500 {
				id := fmt.Sprint(g, "-", i)
				_ = s.SetSession(ctx, &oauth.Session{SessionID: id, TokenSet: oauth.TokenSet{Sub: testDID}})
				_, _ = s.GetSession(ctx, testDID, id)
				_ = s.DeleteSession(ctx, testDID, id)
			}
		}()
	}
	wg.Wait()
	if n := s.len(); n != 0 {
		t.Errorf("%d sessions left", n)
	}
}
