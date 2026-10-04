package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/jcalabro/atmos"
	atmosidentity "github.com/jcalabro/atmos/identity"

	"github.com/haileyok/topic-feed/internal/feedgen"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// This test binary links everything the service does, sign-in included, so it is where a
// library that replaces the JWT signing methods other code relies on would show up.
func TestCredentialsCanBeCheckedWithEverythingTheServiceLinks(t *testing.T) {
	if err := feedgen.CheckCredentials(); err != nil {
		t.Fatal(err)
	}
}

func TestSignInIsOffWithoutASecret(t *testing.T) {
	t.Setenv("FEEDGEN_SESSION_SECRET", "")
	h, handle, err := newSignIn("https://feeds.example.test", quiet)
	if h != nil || handle != nil || err != nil {
		t.Errorf("%v, %v: with no secret the page is simply off", h, err)
	}
}

func TestSignInRefusesAWeakSecret(t *testing.T) {
	for _, secret := range []string{"x", "too short for signing cookies", strings.Repeat("a", 31)} {
		t.Setenv("FEEDGEN_SESSION_SECRET", secret)
		h, _, err := newSignIn("https://feeds.example.test", quiet)
		if h != nil || err == nil || !strings.Contains(err.Error(), "FEEDGEN_SESSION_SECRET") {
			t.Errorf("%q: %v, %v: a secret that is set but too weak is an error, not a quiet 'off'", secret, h, err)
		}
	}
}

func TestSignInOnWithASecret(t *testing.T) {
	t.Setenv("FEEDGEN_SESSION_SECRET", strings.Repeat("s", 32))
	h, handle, err := newSignIn("https://feeds.example.test", quiet)
	if err != nil || h == nil || handle == nil {
		t.Fatalf("%v, %v, %v", h, handle != nil, err)
	}
	if h.Origin() != "https://feeds.example.test" {
		t.Errorf("origin %q", h.Origin())
	}
	// Its client metadata names the address it is served from, as OAuth requires.
	w := httptest.NewRecorder()
	h.ServeMetadata(w, httptest.NewRequest("GET", "/oauth/client-metadata.json", nil))
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"client_id":"https://feeds.example.test/oauth/client-metadata.json"`) ||
		!strings.Contains(w.Body.String(), `"redirect_uris":["https://feeds.example.test/oauth/callback"]`) {
		t.Errorf("%d %s", w.Code, w.Body.String())
	}
	// Logins are rate limited per visitor: a burst, then refused.
	var refused bool
	for range 20 {
		r := httptest.NewRequest("POST", "/oauth/login", strings.NewReader("handle="))
		r.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		r.Header.Set("CF-Connecting-IP", "198.51.100.7")
		w := httptest.NewRecorder()
		h.ServeLogin(w, r)
		if w.Code == http.StatusTooManyRequests {
			refused = true
			break
		}
	}
	if !refused {
		t.Error("twenty logins in a row from one address were all allowed")
	}
}

func TestSignInRefusesABadOrigin(t *testing.T) {
	t.Setenv("FEEDGEN_SESSION_SECRET", strings.Repeat("s", 32))
	for _, origin := range []string{"http://feeds.example.test", "https://feeds.example.test/path", "feeds.example.test", "https://"} {
		if h, _, err := newSignIn(origin, quiet); err == nil || h != nil {
			t.Errorf("%q: %v, %v", origin, h, err)
		}
	}
}

type handleResolver struct {
	handles map[string]atmos.DID
	docs    map[string]*atmosidentity.DIDDocument
}

func (r *handleResolver) ResolveDID(_ context.Context, did atmos.DID) (*atmosidentity.DIDDocument, error) {
	if d, ok := r.docs[string(did)]; ok {
		return d, nil
	}
	return nil, errors.New("no such DID")
}

func (r *handleResolver) ResolveHandle(_ context.Context, h atmos.Handle) (atmos.DID, error) {
	if d, ok := r.handles[string(h)]; ok {
		return d, nil
	}
	return "", errors.New("no such handle")
}

func TestHandleLookup(t *testing.T) {
	doc := func(did, handle string) *atmosidentity.DIDDocument {
		return &atmosidentity.DIDDocument{ID: did, AlsoKnownAs: []string{"at://" + handle}, Service: []atmosidentity.Service{
			{ID: "#atproto_pds", Type: "AtprotoPersonalDataServer", ServiceEndpoint: "https://pds.example.test"}}}
	}
	const alice, bob = "did:plc:alicealicealicealicealic", "did:plc:bobbobbobbobbobbobbobbob"
	res := &handleResolver{
		handles: map[string]atmos.DID{"alice.example.test": alice}, // bob's handle doesn't point back at him
		docs:    map[string]*atmosidentity.DIDDocument{alice: doc(alice, "alice.example.test"), bob: doc(bob, "bob.example.test")},
	}
	lookup := handleLookup(&atmosidentity.Directory{Resolver: res})
	ctx := context.Background()
	if got := lookup(ctx, alice); got != "alice.example.test" {
		t.Errorf("alice: %q", got)
	}
	if got := lookup(ctx, bob); got != "" {
		t.Errorf("bob's handle doesn't check out, so there is none to show: %q", got)
	}
	if got := lookup(ctx, "did:plc:nobodynobodynobodynobod"); got != "" {
		t.Errorf("an account that can't be found: %q", got)
	}
}
