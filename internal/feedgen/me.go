package feedgen

import (
	"context"
	"encoding/json"
	"log/slog"
	"net/http"
	"strconv"
	"sync"
	"time"
)

const (
	// interestsTimeout bounds one reading of a viewer's likes: it is several database reads.
	interestsTimeout = 25 * time.Second
	handleTimeout    = 3 * time.Second
)

// MeAPI answers a signed-in viewer's questions about their own personal feed. Every answer is
// about the account that is signed in on the request, found from the signed cookie and nothing
// else: no parameter, header, or path says whose data it is.
type MeAPI struct {
	// Viewer says who is signed in on a request, if anyone (signin.Handler.Viewer).
	Viewer func(r *http.Request) (did string, ok bool)
	// Handle finds an account's handle, "" if it has none that checks out; nil: none is shown.
	Handle func(ctx context.Context, did string) string
	// Interests turns a viewer's likes into what their feed is built from.
	Interests *InterestsBuilder
	// Tunings holds how viewers have adjusted their feeds.
	Tunings TuningStore
	// Reads limits how often one viewer can ask for their interests, which reads the database
	// (it is keyed by the viewer's DID).
	Reads *IPLimiter
	Log   *slog.Logger

	// Origin is this site's origin (https://host): saving a tuning must come from it.
	Origin string
	// Personal is the feed service that applies a saved tuning and previews a draft one;
	// Feed is the rkey of the personal feed the page is about.
	Personal *Personal
	Feed     string
	// Ranking is how that feed ranks posts, which the viewer's ranking settings start from.
	Ranking Ranking
	// Texts reads the text of posts shown in the preview.
	Texts PostTextSource
	// Edits limits how often one viewer can read or save their tuning, and Previews how often
	// they can preview a draft (a preview assembles a whole feed); both are keyed by DID.
	Edits    *IPLimiter
	Previews *IPLimiter

	offerOnce sync.Once
	offered   []TopicChoice
	offeredBy map[string]bool
}

func meHeaders(w http.ResponseWriter) {
	h := w.Header()
	h.Set("Cache-Control", "no-store")
	h.Set("X-Content-Type-Options", "nosniff")
}

func meJSON(w http.ResponseWriter, status int, v any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(v)
}

// signedIn answers 401 and returns false if nobody is signed in.
func (m *MeAPI) signedIn(w http.ResponseWriter, r *http.Request) (string, bool) {
	meHeaders(w)
	did, ok := m.Viewer(r)
	if !ok {
		meJSON(w, http.StatusUnauthorized, map[string]string{"error": "not signed in"})
		return "", false
	}
	return did, true
}

// ServeInterests answers GET /api/me/interests: what the signed-in viewer's likes say they are
// into, and how their saved tuning changes that, with the liked posts behind each interest.
func (m *MeAPI) ServeInterests(w http.ResponseWriter, r *http.Request) {
	did, ok := m.signedIn(w, r)
	if !ok {
		return
	}
	if m.Reads != nil && !m.Reads.Allow(did) {
		w.Header().Set("Retry-After", strconv.Itoa(5))
		meJSON(w, http.StatusTooManyRequests, map[string]string{"error": "limited"})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), interestsTimeout)
	defer cancel()
	// Showing their interests without the tuning they saved would be wrong in a way that looks
	// right, so if it can't be read there is no answer.
	tuning, err := m.viewerTuning(ctx, did)
	if err != nil {
		m.Log.Error("me: reading a viewer's tuning failed", "viewer", did, "err", err)
		meJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
		return
	}
	handle := ""
	if m.Handle != nil {
		hctx, hcancel := context.WithTimeout(ctx, handleTimeout)
		handle = m.Handle(hctx, did)
		hcancel()
	}
	resp, err := m.Interests.Build(ctx, did, handle, tuning)
	if err != nil {
		m.Log.Error("me: reading a viewer's interests failed", "viewer", did, "err", err)
		meJSON(w, http.StatusServiceUnavailable, map[string]string{"error": "unavailable"})
		return
	}
	meJSON(w, http.StatusOK, resp)
}
