// Command images keeps the pictures of the posts Jev labeled (internal/imagearchive):
// resolve asks the AppView where each post's pictures are, fetch downloads them into
// a content-addressed directory, purge removes the pictures of posts that were
// deleted, and stats prints what the archive holds.
//
// Configuration comes from environment variables:
//
//	CLICKHOUSE_ADDR       default localhost:9000
//	CLICKHOUSE_DB         default topicfeed
//	CLICKHOUSE_USER       default topicfeed
//	CLICKHOUSE_PASSWORD   required
//
// Flags:
//
//	-root        archive root, default /data/images
//	-policy      feed policy file, default config/label_policy.yaml
//	-appview     AppView base URL, default https://public.api.bsky.app
//	-rps         getPosts calls per second, default 4
//	-workers     parallel downloads, default 24
//	-fetch-rps   downloads per second, default 60
//	-limit       posts (resolve) or pictures (fetch) to do this run, 0 for all
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"syscall"

	"github.com/haileyok/topic-feed/internal/chdb"
	"github.com/haileyok/topic-feed/internal/imagearchive"
	"github.com/haileyok/topic-feed/internal/labelpolicy"
)

func main() {
	log := slog.New(slog.NewTextHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	var (
		root    = flag.String("root", "/data/images", "archive root")
		policy  = flag.String("policy", "config/label_policy.yaml", "feed policy file")
		appview = flag.String("appview", "https://public.api.bsky.app", "AppView base URL")
		rps     = flag.Float64("rps", 4, "getPosts calls per second")
		workers = flag.Int("workers", 72, "parallel downloads")
		fetch   = flag.Float64("fetch-rps", 60, "downloads per second")
		limit   = flag.Int("limit", 0, "posts/pictures to do this run, 0 for all")
	)
	flag.Parse()
	if flag.NArg() != 1 {
		fmt.Fprintln(os.Stderr, "usage: images [flags] resolve|fetch|purge|stats")
		os.Exit(2)
	}
	if err := run(log, flag.Arg(0), *root, *policy, *appview, *rps, *workers, *fetch, *limit); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("images stopped", "cmd", flag.Arg(0), "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger, cmd, root, policyPath, appviewURL string, rps float64, workers int, fetchRPS float64, limit int) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	if os.Getenv("CLICKHOUSE_PASSWORD") == "" {
		return errors.New("CLICKHOUSE_PASSWORD must be set")
	}
	conn, err := chdb.Open(ctx, chdb.FromEnv())
	if err != nil {
		return err
	}
	defer conn.Close()
	store := &imagearchive.Store{Conn: conn}

	switch cmd {
	case "stats":
		st, err := store.Stats(ctx)
		if err != nil {
			return err
		}
		fmt.Print(st)
		return nil
	case "resolve":
		pol, err := labelpolicy.Load(policyPath)
		if err != nil {
			return err
		}
		res := &imagearchive.Resolver{
			Store:   store,
			Policy:  pol,
			Log:     log,
			RPS:     rps,
			AppView: &imagearchive.AppView{BaseURL: appviewURL},
		}
		return res.Run(ctx, limit)
	case "fetch":
		f := &imagearchive.Fetcher{
			Store:   store,
			Root:    root,
			Log:     log,
			Workers: workers,
			RPS:     fetchRPS,
		}
		return f.Run(ctx, limit)
	case "purge":
		p := &imagearchive.Purger{Store: store, Root: root, Log: log}
		rows, files, err := p.Run(ctx)
		if err != nil {
			return err
		}
		log.Info("purge finished", "rows", rows, "files", files)
		return nil
	default:
		return fmt.Errorf("unknown command %q (resolve, fetch, purge, stats)", cmd)
	}
}
