// Command feedgen serves the topic feeds to Bluesky, and publishes their records.
//
//	feedgen [serve]            serve the feeds (default)
//	feedgen publish [-dry-run] [-code C]
//	                           write an app.bsky.feed.generator record for every feed in
//	                           the config into the owner's repo (-dry-run: print them;
//	                           -code: the emailed sign-in code, only needed when logging
//	                           in with the account's main password and email 2FA is on).
//	                           At a terminal it shows the records, asks before writing,
//	                           and asks for the sign-in code if Bluesky sends one.
//
// Configuration comes from environment variables:
//
//	FEEDGEN_HOSTNAME          public hostname the service is reached at over HTTPS (required)
//	FEEDGEN_SERVICE_DID       the service's DID, default did:web:<FEEDGEN_HOSTNAME>
//	FEEDGEN_OWNER_DID         account that owns the feed records (required)
//	FEEDGEN_CONFIG            feed list, default config/feeds.yaml
//	TAXONOMY                  default taxonomy/v1.yaml (feed paths are checked against it)
//	LABEL_POLICY              default config/label_policy.yaml
//	FEEDGEN_WINDOW_HOURS      how far back feeds go, default 48
//	FEEDGEN_REFRESH_SECONDS   how often feeds are rebuilt, default 20
//	FEEDGEN_MAX_POSTS         posts per feed, default 3000
//	HTTP_ADDR                 public listener, default :8710
//	METRICS_ADDR              Prometheus metrics, default :9104
//	LOG_LEVEL                 debug|info|warn, default info
//	CLICKHOUSE_ADDR, CLICKHOUSE_DB, CLICKHOUSE_USER, CLICKHOUSE_PASSWORD (serve only)
//
// publish also needs:
//
//	FEEDGEN_HANDLE            login handle or DID of the owner, default FEEDGEN_OWNER_DID
//	FEEDGEN_APP_PASSWORD      an app password for that account (not needed with -dry-run)
package main

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/bluesky-social/indigo/atproto/atclient"
	"github.com/bluesky-social/indigo/atproto/identity"
	"github.com/bluesky-social/indigo/atproto/syntax"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/haileyok/topic-feed/internal/chdb"
	"github.com/haileyok/topic-feed/internal/feedgen"
	"github.com/haileyok/topic-feed/internal/labelpolicy"
	"github.com/haileyok/topic-feed/internal/taxonomy"
)

func main() {
	var level slog.Level
	if err := level.UnmarshalText([]byte(env("LOG_LEVEL", "info"))); err != nil {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	cmd, args := "serve", os.Args[1:]
	if len(args) > 0 && (args[0] == "serve" || args[0] == "publish") {
		cmd, args = args[0], args[1:]
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	var err error
	switch cmd {
	case "serve":
		err = serve(ctx, log)
	case "publish":
		fs := flag.NewFlagSet("publish", flag.ExitOnError)
		dry := fs.Bool("dry-run", false, "print the records instead of writing them")
		code := fs.String("code", "", "emailed sign-in code (email 2FA with the main password; app passwords don't need it)")
		fs.Parse(args)
		err = publish(ctx, *dry, *code)
	}
	if err != nil && !errors.Is(err, context.Canceled) {
		log.Error("feedgen "+cmd+" failed", "err", err)
		os.Exit(1)
	}
}

type settings struct {
	hostname   string
	serviceDID string
	owner      syntax.DID
	cfg        *feedgen.Config
	tax        *taxonomy.Taxonomy
}

func load() (*settings, error) {
	s := &settings{hostname: os.Getenv("FEEDGEN_HOSTNAME")}
	if s.hostname == "" {
		return nil, errors.New("FEEDGEN_HOSTNAME must be set")
	}
	s.serviceDID = env("FEEDGEN_SERVICE_DID", "did:web:"+s.hostname)
	if _, err := syntax.ParseDID(s.serviceDID); err != nil {
		return nil, fmt.Errorf("FEEDGEN_SERVICE_DID: %w", err)
	}
	owner, err := syntax.ParseDID(os.Getenv("FEEDGEN_OWNER_DID"))
	if err != nil {
		return nil, fmt.Errorf("FEEDGEN_OWNER_DID: %w", err)
	}
	s.owner = owner
	tax, err := taxonomy.Load(env("TAXONOMY", "taxonomy/v1.yaml"))
	if err != nil {
		return nil, err
	}
	if s.cfg, err = feedgen.LoadConfig(env("FEEDGEN_CONFIG", "config/feeds.yaml"), tax); err != nil {
		return nil, err
	}
	s.tax = tax
	return s, nil
}

func serve(ctx context.Context, log *slog.Logger) error {
	s, err := load()
	if err != nil {
		return err
	}
	policy, err := labelpolicy.Load(env("LABEL_POLICY", "config/label_policy.yaml"))
	if err != nil {
		return err
	}
	if os.Getenv("CLICKHOUSE_PASSWORD") == "" {
		return errors.New("CLICKHOUSE_PASSWORD must be set")
	}

	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		if err := http.ListenAndServe(env("METRICS_ADDR", ":9104"), mux); err != nil {
			log.Error("metrics server", "err", err)
		}
	}()

	conn, err := chdb.Open(ctx, chdb.FromEnv())
	if err != nil {
		return err
	}
	defer conn.Close()

	every := time.Duration(envInt("FEEDGEN_REFRESH_SECONDS", 20)) * time.Second
	feeds := feedgen.NewFeeds(s.cfg, &feedgen.Store{Conn: conn, Policy: policy}, log,
		time.Duration(envInt("FEEDGEN_WINDOW_HOURS", 48))*time.Hour, every, envInt("FEEDGEN_MAX_POSTS", 3000))
	feeds.Start(ctx)

	srv := feedgen.NewServer(feedgen.ServerConfig{
		Hostname: s.hostname, ServiceDID: s.serviceDID, OwnerDID: s.owner,
		MaxAge: max(5*every, 2*time.Minute),
	}, feeds, identity.DefaultDirectory(), log)

	// Interactions are written in the background and flushed after the server stops.
	// The feed builder page at "/" previews feeds with the same store.
	pv := feedgen.NewPreviewer(&feedgen.Store{Conn: conn, Policy: policy}, s.tax, log)
	go pv.Run(ctx)
	srv.Preview = pv

	iw := feedgen.NewInteractionWriter(conn, log, 200_000)
	wctx, stopWriter := context.WithCancel(context.Background())
	go iw.Run(wctx)
	srv.Interactions = iw
	for _, f := range s.cfg.Feeds {
		log.Info("serving feed", "feed", f.Rkey, "uri", srv.FeedURI(f.Rkey), "paths", f.Paths, "min_prob", f.MinProb)
	}

	errc := make(chan error, 1)
	addr := env("HTTP_ADDR", ":8710")
	go func() { errc <- srv.Start(addr) }()
	log.Info("feedgen listening", "addr", addr, "service_did", s.serviceDID, "hostname", s.hostname)

	select {
	case err := <-errc:
		stopWriter()
		iw.Wait()
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = srv.Shutdown(sctx)
	stopWriter()
	iw.Wait()
	return err
}

func publish(ctx context.Context, dry bool, code string) error {
	s, err := load()
	if err != nil {
		return err
	}
	dir := identity.DefaultDirectory()
	p := &feedgen.Publisher{Dir: dir, OwnerDID: s.owner, ServiceDID: s.serviceDID, Out: os.Stdout}
	if dry {
		return p.Publish(ctx, s.cfg.Feeds, nil)
	}
	password := os.Getenv("FEEDGEN_APP_PASSWORD")
	if password == "" {
		return errors.New("FEEDGEN_APP_PASSWORD must be set (or use -dry-run)")
	}
	id, err := syntax.ParseAtIdentifier(env("FEEDGEN_HANDLE", s.owner.String()))
	if err != nil {
		return fmt.Errorf("FEEDGEN_HANDLE: %w", err)
	}

	// At a terminal: show what will be written and ask before logging in.
	in := bufio.NewReader(os.Stdin)
	tty := isTerminal(os.Stdin)
	if tty {
		if err := p.Publish(ctx, s.cfg.Feeds, nil); err != nil {
			return err
		}
		if !strings.EqualFold(prompt(in, "\nPublish these records as "+id.String()+"? [y/N] "), "y") {
			return errors.New("not published")
		}
	}

	login, err := atclient.LoginWithPassword(ctx, dir, id, password, code, nil)
	var apiErr *atclient.APIError
	if errors.As(err, &apiErr) && apiErr.Name == "AuthFactorTokenRequired" {
		if !tty {
			return errors.New("Bluesky emailed a sign-in code (email 2FA): run again at a terminal to be " +
				"asked for it, or pass -code <code> (make feeds-publish CODE=<code>). App passwords don't need one")
		}
		code = prompt(in, "Bluesky emailed you a sign-in code. Enter it: ")
		login, err = atclient.LoginWithPassword(ctx, dir, id, password, code, nil)
	}
	if err != nil {
		return fmt.Errorf("log in: %w", err)
	}
	return p.Publish(ctx, s.cfg.Feeds, login)
}

func prompt(in *bufio.Reader, question string) string {
	fmt.Print(question)
	line, _ := in.ReadString('\n')
	return strings.TrimSpace(line)
}

func isTerminal(f *os.File) bool {
	st, err := f.Stat()
	return err == nil && st.Mode()&os.ModeCharDevice != 0
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if n, err := strconv.Atoi(os.Getenv(key)); err == nil && n > 0 {
		return n
	}
	return def
}
