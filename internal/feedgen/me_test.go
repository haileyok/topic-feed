package feedgen

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/haileyok/topic-feed/internal/chdb"
	"github.com/haileyok/topic-feed/internal/signin"
	"github.com/haileyok/topic-feed/internal/taxonomy"
)

const (
	meDID    = "did:plc:ragtjsm2j2vknwkz3zp4oxrd"
	otherDID = "did:plc:bbbbbbbbbbbbbbbbbbbbbbbb"
)

type meRig struct {
	s        *Server
	sessions *signin.Sessions
	src      *fakeInterests
	tun      *fakeTunings
}

func newMeRig(t *testing.T) *meRig {
	t.Helper()
	s := testServer(t)
	sessions, err := signin.NewSessions([]byte(strings.Repeat("k", 32)), time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	h, err := signin.New(signin.Config{Origin: signInOrigin, Auth: fakeSignInAuth{}, Metadata: signin.ClientMetadataFor(signInOrigin, "Test"),
		Sessions: sessions, Log: log})
	if err != nil {
		t.Fatal(err)
	}
	s.SignIn = h
	src := &fakeInterests{likes: baseLikes, liked: append(likedIn("a/x", 3), likedIn("e/x", 2)...)}
	tun := newTunings("", nil)
	s.Me = &MeAPI{
		Viewer: h.Viewer,
		Handle: func(_ context.Context, did string) string {
			if did == meDID {
				return "alice.example.test"
			}
			return ""
		},
		Interests: interestsBuilder(src, nil),
		Tunings:   tun,
		Reads:     NewIPLimiter(time.Hour, 3), // three reads at once per viewer, none more for an hour
		Log:       log,
	}
	return &meRig{s: s, sessions: sessions, src: src, tun: tun}
}

func serveReq(method, target string, hdr map[string]string, cookies ...*http.Cookie) *http.Request {
	r := httptest.NewRequest(method, target, nil)
	for k, v := range hdr {
		r.Header.Set(k, v)
	}
	for _, c := range cookies {
		r.AddCookie(c)
	}
	return r
}

func serveRec(s *Server, r *http.Request) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	s.Handler().ServeHTTP(w, r)
	return w
}

func (m *meRig) cookie(did string) *http.Cookie {
	return &http.Cookie{Name: "__Host-feeds_session", Value: m.sessions.Issue(did)}
}

func (m *meRig) get(target string, hdr map[string]string, cookies ...*http.Cookie) (int, http.Header, []byte) {
	req := serveReq("GET", target, hdr, cookies...)
	rec := serveRec(m.s, req)
	return rec.Code, rec.Header(), rec.Body.Bytes()
}

func (m *meRig) interests(t *testing.T, did string) (InterestsResponse, int) {
	t.Helper()
	code, _, body := m.get("/api/me/interests", nil, m.cookie(did))
	var r InterestsResponse
	if code == 200 {
		if err := json.Unmarshal(body, &r); err != nil {
			t.Fatalf("%v: %s", err, body)
		}
	}
	return r, code
}

func byPath(r InterestsResponse) map[string]Interest {
	out := map[string]Interest{}
	for _, in := range r.Interests {
		out[in.Path] = in
	}
	return out
}

func TestMeInterestsNeedSignIn(t *testing.T) {
	m := newMeRig(t)
	expired := m.cookie(meDID)
	for name, cookies := range map[string][]*http.Cookie{
		"no cookie":       nil,
		"a made-up value": {{Name: "__Host-feeds_session", Value: "v1.AAAA.9999999999.AAAA"}},
		"the wrong name":  {{Name: "feeds_session", Value: expired.Value}},
		"the DID itself":  {{Name: "__Host-feeds_session", Value: meDID}},
	} {
		code, hdr, body := m.get("/api/me/interests", nil, cookies...)
		if code != http.StatusUnauthorized || hdr.Get("Cache-Control") != "no-store" || !strings.Contains(string(body), "not signed in") {
			t.Errorf("%s: %d %s %v", name, code, body, hdr)
		}
	}
	if reads := m.src.reads(); len(reads) != 0 {
		t.Errorf("someone who isn't signed in caused database reads: %v", reads)
	}
}

func TestMeInterestsAreTheSignedInViewersOwn(t *testing.T) {
	m := newMeRig(t)
	// Whatever else the request says about whose data it wants is ignored.
	code, hdr, body := m.get("/api/me/interests?did="+otherDID+"&viewer="+otherDID+"&handle=bob.test",
		map[string]string{"X-Viewer": otherDID, "X-Did": otherDID, "Authorization": "Bearer " + otherDID}, m.cookie(meDID))
	if code != 200 || hdr.Get("Cache-Control") != "no-store" || !strings.HasPrefix(hdr.Get("Content-Type"), "application/json") {
		t.Fatalf("%d %v %s", code, hdr, body)
	}
	var r InterestsResponse
	if err := json.Unmarshal(body, &r); err != nil {
		t.Fatal(err)
	}
	if r.DID != meDID || r.Handle != "alice.example.test" || !r.Personalized || r.State != StatePersonal || len(r.Interests) != 4 {
		t.Errorf("%+v", r)
	}
	if reads := m.src.reads(); len(reads) != 1 || reads[0] != meDID {
		t.Errorf("likes were read for %v, want only the signed-in account", reads)
	}
	if a := byPath(r)["a/x"]; a.Name != "Alpha" || a.Posts != 3 || len(a.Samples) != 3 || a.Weight != 1 {
		t.Errorf("%+v", a)
	}
}

func TestMeInterestsApplyTheViewersSavedTuning(t *testing.T) {
	m := newMeRig(t)
	if err := m.tun.SaveTuning(context.Background(), meDID, Tuning{Topics: map[string]float64{"a/x": 0, "b/x": 2}}); err != nil {
		t.Fatal(err)
	}
	r, code := m.interests(t, meDID)
	if code != 200 {
		t.Fatalf("%d", code)
	}
	got := byPath(r)
	if a := got["a/x"]; a.Weight != 0 || a.TunedShare != 0 {
		t.Errorf("muted: %+v", a)
	}
	if b := got["b/x"]; b.Weight != 2 || b.TunedShare <= b.Share {
		t.Errorf("turned up: %+v", b)
	}
	// Another viewer's feed isn't touched by it.
	other, code := m.interests(t, otherDID)
	if code != 200 {
		t.Fatalf("%d", code)
	}
	for _, in := range other.Interests {
		if in.Weight != 1 || !near(in.TunedShare, in.Share) {
			t.Errorf("another viewer's interest was changed: %+v", in)
		}
	}
	if other.Handle != "" {
		t.Errorf("handle %q", other.Handle)
	}
}

func TestMeInterestsAreLimitedPerViewer(t *testing.T) {
	m := newMeRig(t)
	for i := range 3 {
		if _, code := m.interests(t, meDID); code != 200 {
			t.Fatalf("read %d: %d", i+1, code)
		}
	}
	code, hdr, body := m.get("/api/me/interests", nil, m.cookie(meDID))
	if code != http.StatusTooManyRequests || hdr.Get("Retry-After") == "" || !strings.Contains(string(body), "limited") {
		t.Errorf("%d %v %s", code, hdr, body)
	}
	if reads := m.src.reads(); len(reads) != 3 {
		t.Errorf("%d reads: a refused request must not read the database", len(reads))
	}
	// Someone else still can.
	if _, code := m.interests(t, otherDID); code != 200 {
		t.Errorf("another viewer: %d", code)
	}
	// And someone who isn't signed in costs nothing against anyone's allowance.
	if code, _, _ := m.get("/api/me/interests", nil); code != http.StatusUnauthorized {
		t.Errorf("signed out: %d", code)
	}
}

func TestMeInterestsFailClosedAndKeepErrorsPrivate(t *testing.T) {
	// If the saved tuning can't be read, showing the interests without it would look right
	// and be wrong.
	m := newMeRig(t)
	m.tun.failReads(errors.New("clickhouse is down: password=hunter2"))
	code, _, body := m.get("/api/me/interests", nil, m.cookie(meDID))
	if code != http.StatusServiceUnavailable || strings.Contains(string(body), "hunter2") || strings.Contains(string(body), "clickhouse") || !strings.Contains(string(body), "unavailable") {
		t.Errorf("tuning unreadable: %d %s", code, body)
	}
	if reads := m.src.reads(); len(reads) != 0 {
		t.Errorf("likes were read anyway: %v", reads)
	}

	m = newMeRig(t)
	m.src.failWith(errors.New("select likes: dial tcp 10.0.0.5:9000: connection refused"))
	code, _, body = m.get("/api/me/interests", nil, m.cookie(meDID))
	if code != http.StatusServiceUnavailable || strings.Contains(string(body), "10.0.0.5") || !strings.Contains(string(body), "unavailable") {
		t.Errorf("likes unreadable: %d %s", code, body)
	}
}

func TestMeInterestsWithoutAHandleLookup(t *testing.T) {
	m := newMeRig(t)
	m.s.Me.Handle = nil
	r, code := m.interests(t, meDID)
	if code != 200 || r.DID != meDID || r.Handle != "" {
		t.Errorf("%d %+v", code, r)
	}
}

func TestMeRoutesAreNotFoundWhileTheyAreOff(t *testing.T) {
	m := newMeRig(t)
	m.s.Me = nil
	if code, _, _ := m.get("/api/me/interests", nil, m.cookie(meDID)); code != http.StatusNotFound {
		t.Errorf("%d", code)
	}
	// And the sign-in routes next to it are unaffected.
	if code, _, _ := m.get("/api/me", nil, m.cookie(meDID)); code != 200 {
		t.Errorf("/api/me: %d", code)
	}
}

// TestMeInterestsAgainstClickHouse runs the real handler on a real database, for one account's
// real likes. It only reads. It needs a server and an account, so it only runs when
// TOPICFEED_CLICKHOUSE_TEST is set (with the CLICKHOUSE_* variables) and TOPICFEED_TEST_DID names
// an account whose likes are in it.
func TestMeInterestsAgainstClickHouse(t *testing.T) {
	did := os.Getenv("TOPICFEED_TEST_DID")
	if os.Getenv("TOPICFEED_CLICKHOUSE_TEST") == "" || did == "" {
		t.Skip("set TOPICFEED_CLICKHOUSE_TEST=1 and TOPICFEED_TEST_DID=did:plc:... (and CLICKHOUSE_PASSWORD) to run this against a database")
	}
	conn, err := chdb.Open(context.Background(), chdb.FromEnv())
	if err != nil {
		t.Fatal(err)
	}
	tax, err := taxonomy.Load("../../taxonomy/v1.yaml")
	if err != nil {
		t.Fatal(err)
	}
	store := &Store{Conn: conn}
	m := newMeRig(t)
	m.s.Me.Interests = &InterestsBuilder{Src: store, Cfg: PersonalConfigDefaults(), Names: TopicNames(tax)}
	m.s.Me.Tunings = store

	r, code := m.interests(t, did)
	if code != 200 {
		t.Fatalf("status %d", code)
	}
	t.Logf("%s: %d likes, %d with topics; %d interests; state %s; took %d ms", r.Handle, r.Coverage.Total, r.Coverage.Classified, len(r.Interests), r.State, r.TookMs)
	if r.DID != did || r.Coverage.Total == 0 {
		t.Fatalf("%+v", r)
	}
	if r.Personalized {
		var share, tuned float64
		for _, in := range r.Interests {
			share += in.Share
			tuned += in.TunedShare
			if in.Name == "" || in.Name == in.Path || in.Broad == "" {
				t.Errorf("an interest has no name from the taxonomy: %+v", in)
			}
			for _, s := range in.Samples {
				if !strings.HasPrefix(s.URL, "https://bsky.app/profile/") {
					t.Errorf("a sample's link is %q", s.URL)
				}
			}
		}
		if !near(share, 1) || !near(tuned, 1) {
			t.Errorf("the shares add up to %v (from likes) and %v (tuned), want 1", share, tuned)
		}
	}
}

func TestMeInterestsOnlyAnswerGET(t *testing.T) {
	m := newMeRig(t)
	for _, method := range []string{"POST", "PUT", "DELETE", "PATCH"} {
		rec := serveRec(m.s, serveReq(method, "/api/me/interests", nil, m.cookie(meDID)))
		if rec.Code != http.StatusMethodNotAllowed {
			t.Errorf("%s: %d", method, rec.Code)
		}
	}
}
