package feedgen

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"github.com/bluesky-social/indigo/atproto/syntax"
)

// accountTimeout bounds an account answer: finding the account, then reading its likes.
const accountTimeout = 20 * time.Second

// ParseAccount reads what the owner typed to name an account: a DID, a handle (with or without @),
// or a link to the account's profile in the Bluesky app. It returns the DID or the handle, and
// whether it is a DID.
func ParseAccount(s string) (id string, isDID bool, err error) {
	s = strings.TrimSpace(s)
	s = strings.TrimPrefix(s, "@")
	for _, prefix := range []string{"https://bsky.app/profile/", "http://bsky.app/profile/", "bsky.app/profile/"} {
		if rest, ok := strings.CutPrefix(s, prefix); ok {
			s, _, _ = strings.Cut(rest, "/")
		}
	}
	if d, err := syntax.ParseDID(s); err == nil {
		return d.String(), true, nil
	}
	if h, err := syntax.ParseHandle(strings.ToLower(s)); err == nil {
		return h.String(), false, nil
	}
	return "", false, errors.New("That isn't a handle, a DID or a link to a profile.")
}

// ServeAccount answers GET /api/inspect/account?account=...: what an account's likes say it is most
// interested in, as its personal feed reads them (the answer /api/me/interests gives that account,
// with the tuning it saved). Only the owner may ask.
func (a *InspectAPI) ServeAccount(w http.ResponseWriter, r *http.Request) {
	meHeaders(w)
	did, ok := a.Viewer(r)
	if !ok {
		inspectError(w, http.StatusUnauthorized, "not signed in", "")
		return
	}
	if did != a.Owner {
		inspectError(w, http.StatusForbidden, "forbidden", "")
		return
	}
	if a.Interests == nil {
		inspectError(w, http.StatusNotFound, "off", "There's no personal feed to read interests for.")
		return
	}
	if a.Limit != nil && !a.Limit.Allow(did) {
		w.Header().Set("Retry-After", "2")
		inspectError(w, http.StatusTooManyRequests, "limited", "")
		return
	}
	id, isDID, err := ParseAccount(r.URL.Query().Get("account"))
	if err != nil {
		inspectError(w, http.StatusBadRequest, "invalid", err.Error())
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), accountTimeout)
	defer cancel()
	account, handle := id, ""
	if !isDID {
		if a.Resolve == nil {
			inspectError(w, http.StatusBadRequest, "invalid", "Use the account's DID: handles can't be looked up here.")
			return
		}
		account, err = a.Resolve(ctx, id)
		switch {
		case errors.Is(err, ErrNoSuchHandle):
			inspectError(w, http.StatusNotFound, "no_account", "No account has that handle.")
			return
		case err != nil:
			a.Log.Warn("inspect account: looking up a handle failed", "handle", id, "err", err)
			inspectError(w, http.StatusBadGateway, "unavailable", "Couldn't look up that handle. Try again, or use the DID.")
			return
		}
		handle = id
	} else if a.Handle != nil {
		hctx, hcancel := context.WithTimeout(ctx, handleTimeout)
		handle = a.Handle(hctx, account)
		hcancel()
	}
	var tuning Tuning
	if a.Tunings != nil {
		if tuning, err = a.Tunings.ViewerTuning(ctx, account); err != nil {
			a.Log.Error("inspect account: reading a tuning failed", "account", account, "err", err)
			inspectError(w, http.StatusServiceUnavailable, "unavailable", "")
			return
		}
	}
	resp, err := a.Interests.Build(ctx, account, handle, tuning)
	if err != nil {
		a.Log.Error("inspect account: reading interests failed", "account", account, "err", err)
		inspectError(w, http.StatusServiceUnavailable, "unavailable", "")
		return
	}
	meJSON(w, http.StatusOK, resp)
}
