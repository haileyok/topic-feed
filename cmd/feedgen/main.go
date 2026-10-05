// Command feedgen serves the topic feeds to Bluesky, and publishes their records.
//
//	feedgen [serve]            serve the feeds (default)
//	feedgen publish [-dry-run] [-code C]
//	                           write an app.bsky.feed.generator record for each personal and
//	                           filtered feed in the config into the owner's repo (topic feeds
//	                           are published from /feeds) (-dry-run: print them;
//	                           -code: the emailed sign-in code, only needed when logging
//	                           in with the account's main password and email 2FA is on).
//	                           At a terminal it shows the records, asks before writing,
//	                           and asks for the sign-in code if Bluesky sends one.
//	feedgen welcome [-dry-run] [-code C] [-text T] [-days-ago N]
//	                           post the welcome message personal feeds show while a viewer's
//	                           feed is being built, as the owner (a real, public post), and
//	                           print the FEEDGEN_WELCOME_POST line that points feeds at it.
//	                           The post is dated N days back (default 90) so it sorts that far
//	                           down followers' timelines rather than at the top. Asks before
//	                           posting, like publish.
//
// Configuration comes from environment variables:
//
//	FEEDGEN_HOSTNAME          public hostname the service is reached at over HTTPS (required)
//	FEEDGEN_SERVICE_DID       the service's DID, default did:web:<FEEDGEN_HOSTNAME>
//	FEEDGEN_OWNER_DID         account that owns the feed records (required)
//	FEEDGEN_CONFIG            feed list, default config/feeds.yaml
//	TAXONOMY                  default taxonomy/v2.1.yaml (feed paths are checked against it)
//	LABEL_POLICY              default config/label_policy.yaml
//	FEEDGEN_WINDOW_HOURS      how far back feeds go, default 24
//	FEEDGEN_REFRESH_SECONDS   how often feeds are rebuilt, default 20
//	FEEDGEN_MAX_POSTS         posts per feed, default 3000
//	FEEDGEN_WELCOME_POST      at:// URI of the post personal feeds show while a viewer's feed
//	                          is being built (create it with `feedgen welcome`); unset: an empty feed
//	FEEDGEN_SESSION_SECRET    at least 32 random characters that sign the cookie saying who is
//	                          signed in on the page at /me (sign-in with Bluesky); unset: sign-in is off
//	FEEDGEN_FILTER_SECRET     at least 32 random characters for the filtered feeds' sign-in: the key the
//	                          service proves itself to viewers' servers with, and the key their kept
//	                          sign-ins are sealed with (changing it signs everyone out); unset: every
//	                          viewer of a filtered feed gets only the sign-in post
//	FEEDGEN_FILTER_SIGNIN_POST at:// URI of the post filtered feeds show, alone, to viewers who haven't
//	                          signed in for them (create it with `feedgen welcome -for filtered`)
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
	if len(args) > 0 && (args[0] == "serve" || args[0] == "publish" || args[0] == "welcome") {
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
	case "welcome":
		fs := flag.NewFlagSet("welcome", flag.ExitOnError)
		dry := fs.Bool("dry-run", false, "print the post instead of writing it")
		code := fs.String("code", "", "emailed sign-in code (email 2FA with the main password; app passwords don't need it)")
		text := fs.String("text", "", "the post's text (default: the one for -for)")
		kind := fs.String("for", "personal", "personal: the post personal feeds show while a viewer's feed is built; "+
			"filtered: the post filtered feeds show viewers who haven't signed in for them")
		daysAgo := fs.Int("days-ago", defaultDaysAgo, "date the post this many days back, so it sorts that far down followers' timelines instead of at the top")
		fs.Parse(args)
		err = welcome(ctx, *dry, *code, *text, *daysAgo, *kind)
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
	tax, err := taxonomy.Load(env("TAXONOMY", "taxonomy/v2.1.yaml"))
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
	// Refuse to start if viewers' credentials can't be checked, rather than serve every
	// viewer as if they were anonymous.
	if err := feedgen.CheckCredentials(); err != nil {
		return err
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
	store := &feedgen.Store{Conn: conn, Policy: policy}
	// The feeds served are the ones in the database (schema/014_user_feeds.sql), which people change
	// on the web. The first time, those of the config file are copied in; after that the file's
	// topic feeds are not looked at, only its personal feed (which is made from each viewer's likes).
	owner := s.owner.String()
	seeded, err := store.SeedFeeds(ctx, owner, s.cfg.Feeds)
	if err != nil {
		return fmt.Errorf("copying the feeds of the config file into the database (apply schema/014_user_feeds.sql with `make schema`): %w", err)
	}
	if seeded > 0 {
		log.Info("copied the feeds of the config file into the database", "feeds", seeded)
	}
	taxonomyPaths := feedgen.TaxonomyPaths(s.tax)
	loadFeeds := func(ctx context.Context) ([]feedgen.Feed, error) {
		return feedgen.LoadServedFeeds(ctx, store, s.cfg, owner, taxonomyPaths, log)
	}
	toServe, err := loadFeeds(ctx)
	if err != nil {
		return err
	}
	feeds := feedgen.NewFeeds(&feedgen.Config{Feeds: toServe}, store, log,
		time.Duration(envInt("FEEDGEN_WINDOW_HOURS", 24))*time.Hour, every, envInt("FEEDGEN_MAX_POSTS", 3000))
	feeds.Start(ctx)
	// Feeds saved on the web appear within this long (sooner when the service itself saved them).
	go feeds.SyncFrom(ctx, loadFeeds, 30*time.Second)

	srv := feedgen.NewServer(feedgen.ServerConfig{
		Hostname: s.hostname, ServiceDID: s.serviceDID, OwnerDID: s.owner,
		MaxAge: max(5*every, 2*time.Minute),
	}, feeds, serviceAuthDirectory(), log)

	// Interactions are written in the background and flushed after the server stops.
	// The feed builder page at "/" previews feeds with the same store.
	pv := feedgen.NewPreviewer(&feedgen.Store{Conn: conn, Policy: policy}, s.tax, log)
	// The owner's private key for adult previews (visit /adult-access?key=...); unset: off.
	pv.AdultKey = os.Getenv("FEEDGEN_BUILDER_ADULT_KEY")
	if pv.AdultKey != "" && len(pv.AdultKey) < 24 {
		return errors.New("FEEDGEN_BUILDER_ADULT_KEY must be at least 24 characters")
	}
	go pv.Run(ctx)
	srv.Preview = pv

	iw := feedgen.NewInteractionWriter(conn, log, 200_000)
	wctx, stopWriter := context.WithCancel(context.Background())
	go iw.Run(wctx)
	srv.Interactions = iw

	// Filtered feeds (`filtered:` in the config): another feed's posts, read as each viewer with a
	// sign-in this service keeps. Their sources are looked up first: the sign-in asks permission
	// for exactly those.
	// The posts they leave out of each viewer's feed are kept a week, for the viewer to look back at.
	leftOut := feedgen.NewLeftOutWriter(conn, log)
	go leftOut.Run(wctx)
	filtered, connector, err := newFiltered(ctx, s, store, feeds, leftOut, log)
	if err != nil {
		stopWriter()
		iw.Wait()
		leftOut.Wait()
		return err
	}
	srv.Filtered = filtered

	// Signing in with Bluesky, for the pages where viewers see and tune their feeds. The filtered
	// feeds' sign-in (which keeps the viewer's sign-in) is one of its routes.
	signIn, handleOf, err := newSignIn("https://"+s.hostname, s.owner.String(), connector, log)
	if err != nil {
		stopWriter()
		iw.Wait()
		return err
	}
	if signIn == nil {
		log.Info("sign-in is off: FEEDGEN_SESSION_SECRET is not set")
	} else if filtered != nil {
		srv.FilteredAPI = newFilteredAPI(s, signIn, connector, store, feeds, filtered, taxonomyPaths, log)
	}

	// Personal feeds (`personal:` in the config): built per viewer from their likes. What
	// they were sent is stored so a restart doesn't forget it.
	var served *feedgen.RowWriter[feedgen.ServedRow]
	if hasPersonal(s.cfg) {
		personal := feedgen.NewPersonal(s.cfg.Feeds, store, log)
		personal.Tunings = store // how viewers have tuned their feeds (schema/010_viewer_settings.sql)
		personal.Welcome = os.Getenv("FEEDGEN_WELCOME_POST")
		if err := checkWelcomePost(personal.Welcome); err != nil {
			stopWriter()
			iw.Wait()
			return err
		}
		if personal.Welcome == "" {
			log.Warn("FEEDGEN_WELCOME_POST is not set: a viewer whose feed is still being built sees an empty feed (make feeds-welcome creates the post)")
		}
		if signIn != nil {
			srv.SignIn = signIn
			srv.Me = newMeAPI(s.cfg, s.tax, store, personal, signIn, "https://"+s.hostname, handleOf, log)
			// The post inspector (/inspect) is the owner's: it is signed in with the same session.
			srv.Inspect = newInspectAPI(s.owner.String(), signIn, store, feeds, policy, s.tax, handleOf, log)
			// The account inspector (/inspect/account) reads an account's interests as its personal feed does.
			if srv.Me != nil {
				srv.Inspect.Interests, srv.Inspect.Tunings = srv.Me.Interests, store
			}
			// People's own feeds (/feeds): made and changed here, published from their browsers.
			srv.FeedsAPI = newFeedsAPI(s.owner.String(), s.serviceDID, "https://"+s.hostname, signIn, store, feeds, taxonomyPaths, log)
			srv.ResolveHandle = newResolveHandleAPI(log)
		}
		served = feedgen.NewServedWriter(conn, log)
		go served.Run(wctx)
		personal.Served = served
		personal.Start(ctx)
		srv.Personal = personal
	} else if signIn != nil && filtered != nil {
		srv.SignIn = signIn // the filtered feeds' page needs to know who is signed in
	}
	stopWriters := func() {
		stopWriter()
		iw.Wait()
		leftOut.Wait()
		if served != nil {
			served.Wait()
		}
	}
	for _, f := range feeds.List() {
		if f.Personal != nil {
			log.Info("serving personal feed", "feed", f.Rkey, "uri", srv.FeedURI(f.Rkey))
			continue
		}
		if f.Filtered != nil {
			log.Info("serving filtered feed", "feed", f.Rkey, "uri", srv.FeedURI(f.Rkey), "source", f.Filtered.Source)
			continue
		}
		log.Info("serving feed", "feed", f.Rkey, "uri", srv.FeedURI(f.Rkey), "paths", f.Paths, "min_prob", f.MinProb)
	}

	errc := make(chan error, 1)
	addr := env("HTTP_ADDR", ":8710")
	go func() { errc <- srv.Start(addr) }()
	log.Info("feedgen listening", "addr", addr, "service_did", s.serviceDID, "hostname", s.hostname)

	select {
	case err := <-errc:
		stopWriters()
		return err
	case <-ctx.Done():
	}
	log.Info("shutting down")
	sctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	err = srv.Shutdown(sctx)
	stopWriters()
	return err
}

func hasPersonal(cfg *feedgen.Config) bool {
	for _, f := range cfg.Feeds {
		if f.Personal != nil {
			return true
		}
	}
	return false
}

// checkWelcomePost accepts "" (no welcome post) or the at:// URI of a post.
func checkWelcomePost(uri string) error { return checkPostURI("FEEDGEN_WELCOME_POST", uri) }

// checkPostURI accepts "" or the at:// URI of a post, for the setting name.
func checkPostURI(name, uri string) error {
	if uri == "" {
		return nil
	}
	u, err := syntax.ParseATURI(uri)
	if err != nil || u.Collection().String() != "app.bsky.feed.post" || u.RecordKey().String() == "" {
		return fmt.Errorf("%s must be the at:// URI of a post (at://did:plc:.../app.bsky.feed.post/...), got %q", name, uri)
	}
	return nil
}

func publish(ctx context.Context, dry bool, code string) error {
	s, err := load()
	if err != nil {
		return err
	}
	dir := identity.DefaultDirectory()
	p := &feedgen.Publisher{Dir: dir, OwnerDID: s.owner, ServiceDID: s.serviceDID, Out: os.Stdout}
	feeds, err := ownerFeeds(ctx, s)
	if err != nil {
		return err
	}
	if dry {
		return p.Publish(ctx, feeds, nil)
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
		if err := p.Publish(ctx, feeds, nil); err != nil {
			return err
		}
		if !strings.EqualFold(prompt(in, "\nPublish these records as "+id.String()+"? [y/N] "), "y") {
			return errors.New("not published")
		}
	}

	login, err := ownerLogin(ctx, dir, id, password, code, in, tty, "make feeds-publish")
	if err != nil {
		return err
	}
	return p.Publish(ctx, feeds, login)
}

// ownerFeeds are the feeds `publish` writes: the personal and filtered feeds of the config file. The
// topic feeds are made and changed on the web, and their records are written from the page at /feeds
// (the owner's included), so publishing here leaves them alone.
func ownerFeeds(_ context.Context, s *settings) ([]feedgen.Feed, error) {
	var mine []feedgen.Feed
	for _, f := range s.cfg.Feeds {
		if f.Personal != nil || f.Filtered != nil {
			mine = append(mine, f)
		}
	}
	if len(mine) == 0 {
		return nil, errors.New("the config has no personal or filtered feeds to publish (topic feeds are published from /feeds)")
	}
	return mine, nil
}

// ownerLogin logs in as the feeds' owner. When Bluesky emails a sign-in code (email 2FA),
// it asks for it at a terminal; otherwise it says how to pass it with makeTarget.
func ownerLogin(ctx context.Context, dir identity.Directory, id syntax.AtIdentifier, password, code string,
	in *bufio.Reader, tty bool, makeTarget string) (*atclient.APIClient, error) {
	login, err := atclient.LoginWithPassword(ctx, dir, id, password, code, nil)
	var apiErr *atclient.APIError
	if errors.As(err, &apiErr) && apiErr.Name == "AuthFactorTokenRequired" {
		if !tty {
			return nil, errors.New("Bluesky emailed a sign-in code (email 2FA): run again at a terminal to be " +
				"asked for it, or pass -code <code> (" + makeTarget + " CODE=<code>). App passwords don't need one")
		}
		code = prompt(in, "Bluesky emailed you a sign-in code. Enter it: ")
		login, err = atclient.LoginWithPassword(ctx, dir, id, password, code, nil)
	}
	if err != nil {
		return nil, fmt.Errorf("log in: %w", err)
	}
	return login, nil
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
