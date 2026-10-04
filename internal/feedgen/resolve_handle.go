package feedgen

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"strconv"
	"time"
)

const resolveHandleTimeout = 8 * time.Second

// ResolveHandleAPI answers com.atproto.identity.resolveHandle, which the page that publishes feeds
// asks to find the account behind a handle someone types: a browser can't look up the handle's DNS
// record itself, and asking Bluesky's own servers would tell them who signs in here.
type ResolveHandleAPI struct {
	// Resolve finds the DID of a handle (ErrNoSuchHandle if there is none).
	Resolve func(ctx context.Context, handle string) (string, error)
	// Limit bounds how often one address can ask: a lookup reaches out to other servers.
	Limit *IPLimiter
	Log   *slog.Logger
}

// ServeResolve answers GET /xrpc/com.atproto.identity.resolveHandle?handle=…, as the lexicon says.
func (a *ResolveHandleAPI) ServeResolve(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("X-Content-Type-Options", "nosniff")
	if a.Limit != nil && !a.Limit.AllowRequest(r) {
		w.Header().Set("Retry-After", strconv.Itoa(5))
		meJSON(w, http.StatusTooManyRequests, xrpcError{Error: "RateLimitExceeded", Message: "slow down"})
		return
	}
	handle := r.URL.Query().Get("handle")
	if handle == "" || len(handle) > 253 {
		meJSON(w, http.StatusBadRequest, xrpcError{Error: "InvalidRequest", Message: "Error: Params must have the property \"handle\""})
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), resolveHandleTimeout)
	defer cancel()
	did, err := a.Resolve(ctx, handle)
	switch {
	case errors.Is(err, ErrNoSuchHandle):
		meJSON(w, http.StatusBadRequest, xrpcError{Error: "InvalidRequest", Message: "Unable to resolve handle"})
	case err != nil || did == "":
		if a.Log != nil {
			a.Log.Warn("resolving a handle failed", "handle", handle, "err", err)
		}
		meJSON(w, http.StatusBadRequest, xrpcError{Error: "InvalidRequest", Message: "Unable to resolve handle"})
	default:
		w.Header().Set("Cache-Control", "public, max-age=60")
		meJSON(w, http.StatusOK, map[string]string{"did": did})
	}
}
