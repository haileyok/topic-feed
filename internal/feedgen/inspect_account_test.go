package feedgen

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"
)

func TestParseAccount(t *testing.T) {
	for in, want := range map[string]struct {
		id    string
		isDID bool
	}{
		"did:plc:abc123":                      {"did:plc:abc123", true},
		" @Alice.Bsky.Social ":                {"alice.bsky.social", false},
		"https://bsky.app/profile/hailey.at":  {"hailey.at", false},
		"bsky.app/profile/did:plc:xyz/post/1": {"did:plc:xyz", true},
	} {
		id, isDID, err := ParseAccount(in)
		if err != nil || id != want.id || isDID != want.isDID {
			t.Errorf("%q: %q %v %v, want %q %v", in, id, isDID, err, want.id, want.isDID)
		}
	}
	for _, bad := range []string{"", "not a handle", "https://example.com/x"} {
		if _, _, err := ParseAccount(bad); err == nil {
			t.Errorf("%q: accepted", bad)
		}
	}
}

func TestAccountInspectorIsTheOwnersAndReadsTheAccountsInterests(t *testing.T) {
	src := &fakeInterests{likes: func(time.Duration) LikeData {
		return LikeData{Mass: map[string]float64{"a/x": 10, "b/x": 5}, Posts: 15}
	}, liked: likedIn("a/x", 3)}
	signedIn := "did:plc:owner"
	api := &InspectAPI{
		Viewer: func(*http.Request) (string, bool) { return signedIn, signedIn != "" },
		Owner:  "did:plc:owner",
		Resolve: func(_ context.Context, h string) (string, error) {
			if h == "alice.test" {
				return "did:plc:alice", nil
			}
			return "", ErrNoSuchHandle
		},
		Interests: interestsBuilder(src, nil),
		// Alice muted Beta: what is shown is her feed as she tuned it.
		Tunings: newTunings("did:plc:alice", &Tuning{Topics: map[string]float64{"b/x": 0}}),
		Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
	}
	ask := func(account string) (int, map[string]any) {
		rec := httptest.NewRecorder()
		api.ServeAccount(rec, httptest.NewRequest(http.MethodGet, "/api/inspect/account?account="+url.QueryEscape(account), nil))
		var body map[string]any
		json.Unmarshal(rec.Body.Bytes(), &body)
		return rec.Code, body
	}

	code, body := ask("@alice.test")
	if code != 200 || body["did"] != "did:plc:alice" || body["handle"] != "alice.test" {
		t.Fatalf("owner asking about alice: %d %v", code, body)
	}
	if reads := src.reads(); len(reads) == 0 || reads[len(reads)-1] != "did:plc:alice" {
		t.Errorf("read the likes of %v, want alice's", reads)
	}
	var beta map[string]any
	for _, r := range body["interests"].([]any) {
		if r.(map[string]any)["path"] == "b/x" {
			beta = r.(map[string]any)
		}
	}
	if beta == nil || beta["weight"] != 0.0 {
		t.Errorf("alice's muted topic: %v", beta)
	}

	if code, _ := ask("nobody.test"); code != http.StatusNotFound {
		t.Errorf("a handle with no account: %d", code)
	}
	if code, _ := ask("what even"); code != http.StatusBadRequest {
		t.Errorf("not an account: %d", code)
	}
	api.Tunings.(*fakeTunings).readErr = errors.New("down")
	if code, _ := ask("did:plc:alice"); code != http.StatusServiceUnavailable {
		t.Errorf("tuning unreadable: %d", code)
	}

	signedIn = "did:plc:someone"
	if code, _ := ask("did:plc:alice"); code != http.StatusForbidden {
		t.Errorf("someone else: %d, want 403", code)
	}
	signedIn = ""
	if code, _ := ask("did:plc:alice"); code != http.StatusUnauthorized {
		t.Errorf("nobody signed in: %d, want 401", code)
	}
}
