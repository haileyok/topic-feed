package signin

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

var testLog = slog.New(slog.NewTextHandler(io.Discard, nil))

// waitMade waits for the Deferred's sign-in to be made, and fails the test if it isn't.
func waitMade(t *testing.T, d *Deferred) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !d.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("the sign-in was never made")
		}
		time.Sleep(time.Millisecond)
	}
}

func TestDeferredWaitsForItsSourcesThenIsTheSignIn(t *testing.T) {
	store := &memLogins{}
	c := testConnector(t, store, nil)
	builds := 0
	d := &Deferred{Wait: time.Millisecond, Log: testLog, Build: func(context.Context) (*Connector, error) {
		builds++
		if builds < 3 {
			return nil, nil // the sources' services aren't known yet
		}
		return c, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)

	// Nothing to ask permission for yet: the routes answer 503, and the filtered feeds show the
	// sign-in post, which is what a viewer who hasn't signed in sees anyway.
	if d.Ready() {
		t.Fatal("ready before the sources were found")
	}
	if _, _, err := d.Start(ctx, "alice.example.test"); !errors.Is(err, ErrNotReady) {
		t.Errorf("Start before it was made: %v", err)
	}
	if _, err := d.Finish(ctx, "code", "state", ""); !errors.Is(err, ErrNotReady) {
		t.Errorf("Finish before it was made: %v", err)
	}
	if _, err := d.Token(ctx, "did:plc:alice", testAud, FeedSkeletonMethod); !errors.Is(err, ErrNotConnected) {
		t.Errorf("a token before it was made: %v", err)
	}
	if ok, err := d.Connected(ctx, "did:plc:alice"); ok || err != nil {
		t.Errorf("connected before it was made: %v %v", ok, err)
	}
	if err := d.Disconnect(ctx, "did:plc:alice"); err != nil {
		t.Errorf("disconnect before it was made: %v", err)
	}
	if m := d.Metadata(); m.ClientID != "" {
		t.Errorf("metadata before it was made: %+v", m)
	}

	waitMade(t, d)
	if builds < 3 {
		t.Errorf("made after %d tries: the sources were looked up again", builds)
	}
	// Once made, it is the sign-in it was built with.
	if m, want := d.Metadata(), c.Metadata(); m.ClientID != want.ClientID || m.Scope != want.Scope {
		t.Errorf("metadata %+v: want %+v", m, want)
	}
	if _, err := d.Token(ctx, "did:plc:alice", testAud, FeedSkeletonMethod); !errors.Is(err, ErrNotConnected) {
		t.Errorf("a token for a viewer who hasn't signed in: %v", err)
	}
}

func TestDeferredKeepsTryingWhenTheSignInCantBeMade(t *testing.T) {
	c := testConnector(t, &memLogins{}, nil)
	tries := 0
	d := &Deferred{Wait: time.Millisecond, Log: testLog, Build: func(context.Context) (*Connector, error) {
		tries++
		if tries < 4 {
			return nil, errors.New("FEEDGEN_FILTER_SECRET makes no usable key")
		}
		return c, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	go d.Run(ctx)
	waitMade(t, d) // a problem with its own settings is reported, not fatal: it is tried again
}

func TestDeferredStopsWhenTheServiceDoes(t *testing.T) {
	called := make(chan struct{}, 1)
	d := &Deferred{Wait: time.Hour, Log: testLog, Build: func(context.Context) (*Connector, error) {
		select {
		case called <- struct{}{}:
		default:
		}
		return nil, nil
	}}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		d.Run(ctx)
		close(done)
	}()
	select {
	case <-called:
	case <-time.After(5 * time.Second):
		t.Fatal("Run never tried to make the sign-in")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Run kept going after the service stopped")
	}
	if d.Ready() {
		t.Error("ready without a sign-in")
	}
}

// connectRig is a rig whose handler has the filtered feeds' sign-in, and its routes.
func connectRig(t *testing.T, connect Connecter) *rig {
	t.Helper()
	r := newRig(t, func(cfg *Config) { cfg.Connect = connect })
	r.mux.HandleFunc("GET /oauth/connect-metadata.json", r.h.ServeConnectMetadata)
	r.mux.HandleFunc("POST /oauth/connect", r.h.ServeConnect)
	r.mux.HandleFunc("GET /oauth/connect-callback", r.h.ServeConnectCallback)
	return r
}

func TestConnectRoutesWaitForTheSignIn(t *testing.T) {
	// found stands for the sources' services: Build can't make the sign-in until it is true.
	c := testConnector(t, &memLogins{}, nil)
	var found atomic.Bool
	waiting := &Deferred{Wait: time.Millisecond, Log: testLog, Build: func(context.Context) (*Connector, error) {
		if !found.Load() {
			return nil, nil
		}
		return c, nil
	}}
	go waiting.Run(context.Background())
	r := connectRig(t, waiting)

	// The page's script is told why in JSON, and the form is sent back with it in the address, as
	// for every other sign-in problem. "off" is what the page has words for: signing in isn't
	// turned on yet.
	w := r.do(req{method: "POST", target: "/oauth/connect", form: url.Values{"handle": {"alice.example.test"}},
		header: map[string]string{"Accept": "application/json"}})
	if w.Code != http.StatusServiceUnavailable || strings.TrimSpace(w.Body.String()) != `{"error":"off"}` {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
	w = r.do(req{method: "POST", target: "/oauth/connect", form: url.Values{"handle": {"alice.example.test"}}})
	if w.Code != http.StatusSeeOther || !strings.Contains(location(w), "signin=off") {
		t.Errorf("%d %q", w.Code, location(w))
	}
	// The metadata names the permission to ask for, which is the sources' services: there is none
	// to write before they are found.
	if w = r.do(req{method: "GET", target: "/oauth/connect-metadata.json"}); w.Code != http.StatusServiceUnavailable {
		t.Errorf("metadata before it was made: %d", w.Code)
	}
	if w = r.do(req{method: "GET", target: "/oauth/connect-callback", form: url.Values{"state": {"state-123"}}}); w.Code != http.StatusSeeOther ||
		!strings.Contains(location(w), "signin=off") {
		t.Errorf("callback before it was made: %d %q", w.Code, location(w))
	}

	// Once the sources have been found the routes are the sign-in's own.
	found.Store(true)
	waitMade(t, waiting)
	w = r.do(req{method: "GET", target: "/oauth/connect-metadata.json"})
	if w.Code != http.StatusOK {
		t.Fatalf("metadata: %d %s", w.Code, w.Body.String())
	}
	var m map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &m); err != nil {
		t.Fatal(err)
	}
	if m["client_id"] != testOrigin+"/oauth/connect-metadata.json" ||
		!strings.Contains(m["scope"].(string), url.QueryEscape(testAud)) {
		t.Errorf("metadata %v", m)
	}
	// A request with no handle reaches the form's own checks, so the 503 is gone.
	if w = r.do(req{method: "POST", target: "/oauth/connect", form: url.Values{},
		header: map[string]string{"Accept": "application/json"}}); w.Code != http.StatusBadRequest {
		t.Errorf("no handle: %d %s", w.Code, w.Body.String())
	}
}

func TestConnectRoutesAreNotFoundWithoutTheSignIn(t *testing.T) {
	r := connectRig(t, nil)
	if w := r.do(req{method: "GET", target: "/oauth/connect-metadata.json"}); w.Code != http.StatusNotFound {
		t.Errorf("metadata without the sign-in: %d", w.Code)
	}
	if w := r.do(req{method: "POST", target: "/oauth/connect", form: url.Values{"handle": {"alice.example.test"}}}); w.Code != http.StatusNotFound {
		t.Errorf("connect without the sign-in: %d", w.Code)
	}
}
