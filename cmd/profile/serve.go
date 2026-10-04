package main

import (
	"context"
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"log/slog"
	"net"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"

	"github.com/haileyok/topic-feed/internal/chdb"
	"github.com/haileyok/topic-feed/internal/feedgen"
	"github.com/haileyok/topic-feed/internal/labelpolicy"
	"github.com/haileyok/topic-feed/internal/taxonomy"
)

//go:embed web/index.html
var indexHTML string

const (
	cacheFor   = 30 * time.Second
	maxLookups = 4 // database lookups at once
)

// resolver turns a handle or DID into the account's DID and current handle.
type resolver func(ctx context.Context, actor string) (did, handle string, err error)

func directoryResolver() resolver {
	dir := identity.DefaultDirectory()
	return func(ctx context.Context, actor string) (string, string, error) {
		id, err := syntax.ParseAtIdentifier(actor)
		if err != nil {
			return "", "", err
		}
		ident, err := dir.Lookup(ctx, id)
		if err != nil {
			return "", "", err
		}
		return ident.DID.String(), ident.Handle.String(), nil
	}
}

type cached struct {
	at   time.Time
	resp *feedgen.InterestsResponse
}

// pageServer is the local-network page for looking at any account's interests. (The public
// page on the feed service shows only the signed-in account's.)
type pageServer struct {
	builder      *feedgen.InterestsBuilder
	resolve      resolver
	defaultActor string
	now          func() time.Time
	log          *slog.Logger

	slots chan struct{}
	mu    sync.Mutex
	cache map[string]cached
}

func newPageServer(src feedgen.InterestsSource, resolve resolver, cfg feedgen.PersonalConfig, names map[string]feedgen.TopicName, defaultActor string, log *slog.Logger) *pageServer {
	p := &pageServer{resolve: resolve, defaultActor: defaultActor, now: time.Now, log: log,
		slots: make(chan struct{}, maxLookups), cache: map[string]cached{}}
	p.builder = &feedgen.InterestsBuilder{Src: src, Cfg: cfg, Names: names, Now: func() time.Time { return p.now() }}
	return p
}

func (p *pageServer) routes() http.Handler {
	mux := http.NewServeMux()
	mux.HandleFunc("GET /{$}", p.page)
	mux.HandleFunc("GET /api/profile", p.profile)
	return mux
}

func (p *pageServer) page(w http.ResponseWriter, r *http.Request) {
	w.Header().Set("Content-Type", "text/html; charset=utf-8")
	w.Header().Set("Cache-Control", "no-store")
	// The page is one file with its own script and style, and talks only to this server.
	w.Header().Set("Content-Security-Policy",
		"default-src 'none'; script-src 'unsafe-inline'; style-src 'unsafe-inline'; connect-src 'self'; base-uri 'none'; form-action 'none'; frame-ancestors 'none'")
	fmt.Fprint(w, strings.ReplaceAll(indexHTML, "__DEFAULT_ACTOR__", html.EscapeString(p.defaultActor)))
}

func (p *pageServer) fail(w http.ResponseWriter, status int, msg string) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	json.NewEncoder(w).Encode(map[string]string{"error": msg})
}

func (p *pageServer) profile(w http.ResponseWriter, r *http.Request) {
	actor := strings.TrimPrefix(strings.TrimSpace(r.URL.Query().Get("actor")), "@")
	if actor == "" {
		actor = p.defaultActor
	}
	if actor == "" {
		p.fail(w, http.StatusBadRequest, "Enter a handle or DID.")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 40*time.Second)
	defer cancel()

	rctx, rcancel := context.WithTimeout(ctx, 10*time.Second)
	did, handle, err := p.resolve(rctx, actor)
	rcancel()
	if err != nil {
		p.fail(w, http.StatusNotFound, fmt.Sprintf("Couldn't find the account %q.", actor))
		return
	}

	resp, err := p.lookup(ctx, did, handle)
	switch {
	case errors.Is(err, errBusy):
		p.fail(w, http.StatusServiceUnavailable, "Busy with other lookups: try again in a moment.")
		return
	case err != nil:
		p.log.Warn("profile lookup failed", "did", did, "err", err)
		p.fail(w, http.StatusInternalServerError, "Couldn't read that account's likes from the database.")
		return
	}
	w.Header().Set("Content-Type", "application/json")
	w.Header().Set("Cache-Control", "no-store")
	json.NewEncoder(w).Encode(resp)
}

var errBusy = errors.New("too many lookups at once")

// lookup builds the response for an account, reusing one built in the last half minute.
func (p *pageServer) lookup(ctx context.Context, did, handle string) (*feedgen.InterestsResponse, error) {
	now := p.now()
	p.mu.Lock()
	if c, ok := p.cache[did]; ok && now.Sub(c.at) < cacheFor {
		p.mu.Unlock()
		return c.resp, nil
	}
	p.mu.Unlock()

	select {
	case p.slots <- struct{}{}:
		defer func() { <-p.slots }()
	case <-ctx.Done():
		return nil, errBusy
	}
	resp, err := p.builder.Build(ctx, did, handle, feedgen.Tuning{}) // this tool shows likes alone
	if err != nil {
		return nil, err
	}
	p.mu.Lock()
	p.cache[did] = cached{at: now, resp: resp}
	for k, c := range p.cache { // forget what has gone stale
		if now.Sub(c.at) > 10*cacheFor {
			delete(p.cache, k)
		}
	}
	p.mu.Unlock()
	return resp, nil
}

// runServe serves the interests page until the process is stopped.
func runServe(ctx context.Context, addr, did, feedName, configPath, policyPath, taxPath string) error {
	cfg, _, err := loadSettings(feedName, configPath, taxPath)
	if err != nil {
		return err
	}
	tax, err := taxonomy.Load(taxPath)
	if err != nil {
		return err
	}
	policy, err := labelpolicy.Load(policyPath)
	if err != nil {
		return err
	}
	conn, err := chdb.Open(ctx, chdb.FromEnv())
	if err != nil {
		return err
	}
	defer conn.Close()

	// Start from the account named, or this machine's owner: the first thing to look at is your own.
	actor := did
	if actor == "" {
		actor = os.Getenv("FEEDGEN_HANDLE")
	}
	if actor == "" {
		actor = os.Getenv("FEEDGEN_OWNER_DID")
	}
	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	p := newPageServer(&feedgen.Store{Conn: conn, Policy: policy}, directoryResolver(), cfg, feedgen.TopicNames(tax), actor, log)

	ln, err := net.Listen("tcp", addr)
	if err != nil {
		return err
	}
	srv := &http.Server{Handler: p.routes(), ReadHeaderTimeout: 10 * time.Second}
	go func() {
		<-ctx.Done()
		srv.Shutdown(context.Background())
	}()
	fmt.Printf("interests page on http://%s/ (starting with %q)\n", ln.Addr(), actor)
	if err := srv.Serve(ln); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}
