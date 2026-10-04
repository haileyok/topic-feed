package signin

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/jcalabro/atmos/oauth"
)

const (
	// pendingTTL is how long a person has to finish signing in at their own server.
	pendingTTL = 10 * time.Minute
	// maxPending bounds the logins in progress, so starting them in bulk can't use up memory.
	maxPending = 5000
)

// ErrBusy means too many people are signing in at once to start another login.
var ErrBusy = errors.New("too many logins are in progress")

// stateStore holds logins that have started and not come back yet, for atmos. The library's
// own in-memory store isn't safe to share between requests. Entries expire, and there is a
// limit on how many there can be.
type stateStore struct {
	mu      sync.Mutex
	ttl     time.Duration
	max     int
	now     func() time.Time
	entries map[string]stateEntry
	purged  time.Time
}

type stateEntry struct {
	state   *oauth.AuthState
	expires time.Time
}

func newStateStore(ttl time.Duration, max int, now func() time.Time) *stateStore {
	return &stateStore{ttl: ttl, max: max, now: now, entries: map[string]stateEntry{}}
}

// purge drops expired entries, at most once a second.
func (s *stateStore) purge(now time.Time) {
	if now.Sub(s.purged) < time.Second {
		return
	}
	s.purged = now
	for k, e := range s.entries {
		if !now.Before(e.expires) {
			delete(s.entries, k)
		}
	}
}

func (s *stateStore) GetState(_ context.Context, state string) (*oauth.AuthState, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purge(now)
	e, ok := s.entries[state]
	if !ok || !now.Before(e.expires) {
		return nil, oauth.ErrInvalidState
	}
	return e.state, nil
}

func (s *stateStore) SetState(_ context.Context, state string, data *oauth.AuthState) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	now := s.now()
	s.purged = time.Time{} // look for room properly before refusing
	s.purge(now)
	if _, replacing := s.entries[state]; !replacing && len(s.entries) >= s.max {
		return ErrBusy
	}
	s.entries[state] = stateEntry{state: data, expires: now.Add(s.ttl)}
	return nil
}

func (s *stateStore) DeleteState(_ context.Context, state string) error {
	s.mu.Lock()
	delete(s.entries, state)
	s.mu.Unlock()
	return nil
}

func (s *stateStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.entries)
}

// sessionStore holds the tokens atmos gets while a login finishes. They are revoked and
// deleted straight away, so it is only ever briefly occupied. The library's in-memory store
// isn't safe to share between requests.
type sessionStore struct {
	mu       sync.Mutex
	sessions map[sessionKey]*oauth.Session
}

type sessionKey struct{ did, id string }

func newSessionStore() *sessionStore { return &sessionStore{sessions: map[sessionKey]*oauth.Session{}} }

func (s *sessionStore) GetSession(_ context.Context, did, sessionID string) (*oauth.Session, error) {
	s.mu.Lock()
	defer s.mu.Unlock()
	sess, ok := s.sessions[sessionKey{did, sessionID}]
	if !ok {
		return nil, oauth.ErrNoSession
	}
	return sess, nil
}

func (s *sessionStore) SetSession(_ context.Context, session *oauth.Session) error {
	if session == nil || session.TokenSet.Sub == "" || session.SessionID == "" {
		return fmt.Errorf("a session needs a subject DID and a session ID")
	}
	s.mu.Lock()
	s.sessions[sessionKey{session.TokenSet.Sub, session.SessionID}] = session
	s.mu.Unlock()
	return nil
}

func (s *sessionStore) DeleteSession(_ context.Context, did, sessionID string) error {
	s.mu.Lock()
	delete(s.sessions, sessionKey{did, sessionID})
	s.mu.Unlock()
	return nil
}

func (s *sessionStore) len() int {
	s.mu.Lock()
	defer s.mu.Unlock()
	return len(s.sessions)
}
