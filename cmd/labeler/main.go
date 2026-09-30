// Command labeler labels posts with hosted Jev (plan §10) and writes jev_labels and
// jev_requests.
//
//	labeler -taxonomy taxonomy/v1.yaml -windows config/labeling_windows.yaml -report auto
//	labeler -taxonomy taxonomy/v1.yaml -limit 2500        # a spread sample instead
//	labeler -live random -source sample -limit 40000 -from 2026-09-29T05:00:00Z -to 2026-09-30T00:00:00Z
//
// With -windows it labels every not-yet-labeled post in the labeling windows. A rerun
// (or a restart after a crash) picks up where it left off, since posts that already
// have a label under the same taxonomy version and label_config are skipped. Posts
// whose batch failed are retried in up to -rounds rounds before it exits.
//
// Environment: TYPESAFE_API_KEY (the AGW key), TYPESAFE_BASE_URL (default
// https://agw.noclues.net), JEV_MODEL (default jev-1.13.0), and the CLICKHOUSE_*
// variables read by chdb.FromEnv.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"os"
	"os/signal"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	typesafe "github.com/haileyok/typesafe-client/go"

	"github.com/haileyok/topic-feed/internal/chdb"
	"github.com/haileyok/topic-feed/internal/labeler"
	"github.com/haileyok/topic-feed/internal/labelreport"
	"github.com/haileyok/topic-feed/internal/taxonomy"
	"github.com/haileyok/topic-feed/internal/windows"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("labeler failed", "err", err)
		os.Exit(1)
	}
}

type opts struct {
	taxPath, windowsPath, report, source string
	liveMode, liveFrom, liveTo           string
	limit, rounds                        int
	cfg                                  labeler.Config
}

func run(log *slog.Logger) error {
	var o opts
	flag.StringVar(&o.taxPath, "taxonomy", "taxonomy/v1.yaml", "taxonomy YAML file")
	flag.StringVar(&o.windowsPath, "windows", "", "label every post in these labeling windows (config/labeling_windows.yaml)")
	flag.IntVar(&o.limit, "limit", 2500, "without -windows: label a spread sample (or with -live, a live selection) of this many posts")
	flag.StringVar(&o.liveMode, "live", "", `label posts the pipeline classified: "random" or "uncertain" (low confidence or weak topics); needs -from and -to`)
	flag.StringVar(&o.liveFrom, "from", "", "with -live: start of the time range (RFC 3339)")
	flag.StringVar(&o.liveTo, "to", "", "with -live: end of the time range (RFC 3339)")
	flag.StringVar(&o.source, "source", "window", "jev_labels.source value")
	flag.StringVar(&o.report, "report", "", `write the review report when done: a directory, or "auto" for /data/reports/<version>-<label_config>`)
	flag.IntVar(&o.rounds, "rounds", 3, "passes over remaining unlabeled posts (retries failed batches)")
	batchSize := flag.Int("batch", 1, "posts per Jev request")
	topK := flag.Int("topk", 3, "broad topics per post that get a subtopic question")
	minBroadP := flag.Float64("min-broad-p", 0.05, "skip broad candidates below this probability")
	concurrency := flag.Int("concurrency", 16, "batches in flight")
	maxRPM := flag.Int("max-rpm", 500, "Jev requests per minute (our share of the shared account)")
	maxTPS := flag.Int("max-tps", 100_000, "estimated Jev input tokens per second")
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tax, err := taxonomy.Load(o.taxPath)
	if err != nil {
		return err
	}
	o.cfg = labeler.Config{
		Model: envOr("JEV_MODEL", "jev-1.13.0"), BatchSize: *batchSize, TopK: *topK, MinBroadP: *minBroadP,
		Source: o.source, Concurrency: *concurrency, MaxRPM: *maxRPM, MaxTPS: *maxTPS,
	}
	labelConfig := labeler.LabelConfig(tax, o.cfg)

	var ws []windows.Window
	if o.windowsPath != "" {
		if ws, err = windows.Load(o.windowsPath); err != nil {
			return err
		}
	}
	var liveFrom, liveTo time.Time
	if o.liveMode != "" {
		if len(ws) > 0 {
			return errors.New("-live and -windows are exclusive")
		}
		if liveFrom, err = time.Parse(time.RFC3339, o.liveFrom); err != nil {
			return fmt.Errorf("-from: %w", err)
		}
		if liveTo, err = time.Parse(time.RFC3339, o.liveTo); err != nil {
			return fmt.Errorf("-to: %w", err)
		}
	}

	client, err := typesafe.NewClient(
		typesafe.WithAPIKey(os.Getenv("TYPESAFE_API_KEY")),
		typesafe.WithBaseURL(envOr("TYPESAFE_BASE_URL", "https://agw.noclues.net")),
		typesafe.WithModel(o.cfg.Model),
		typesafe.WithTimeout(60*time.Second),
		typesafe.WithHeader("X-Client", "topic-feed"),
		typesafe.WithRetryPolicy(retryPolicy()),
	)
	if err != nil {
		return err
	}
	conn, err := chdb.Open(ctx, chdb.FromEnv())
	if err != nil {
		return err
	}
	defer conn.Close()
	store := &labeler.Store{Conn: conn}
	lb := labeler.New(o.cfg, tax, client, log)

	log.Info("starting", "taxonomy", tax.Version, "label_config", labelConfig, "batch", o.cfg.BatchSize,
		"topk", o.cfg.TopK, "model", o.cfg.Model, "windows", len(ws), "max_rpm", o.cfg.MaxRPM)

	for round := 1; round <= o.rounds; round++ {
		var posts []labeler.Post
		switch {
		case o.liveMode != "":
			posts, err = store.SelectLive(ctx, tax.Version, labelConfig, o.source, o.liveMode, liveFrom, liveTo, o.limit)
		case len(ws) > 0:
			posts, err = store.SelectWindows(ctx, tax.Version, labelConfig, ws)
		default:
			posts, err = store.SelectSpread(ctx, tax.Version, labelConfig, o.limit)
		}
		if err != nil {
			return err
		}
		if len(posts) == 0 {
			log.Info("nothing left to label", "round", round)
			break
		}
		log.Info("round starting", "round", round, "posts", len(posts))
		failed, err := labelAll(ctx, log, lb, store, tax.Version, labelConfig, o.cfg, posts)
		if err != nil {
			return err
		}
		log.Info("round done", "round", round, "failed", failed)
		if failed == 0 || (len(ws) == 0 && o.liveMode == "") {
			break // the spread sample is a one-shot preview
		}
	}

	if o.report != "" {
		dir := o.report
		if dir == "auto" {
			dir = labelreport.DefaultDir(tax, labelConfig)
		}
		rctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 30*time.Minute)
		defer cancel()
		if err := labelreport.Write(rctx, conn, tax, labelConfig, dir, 10); err != nil {
			return fmt.Errorf("report: %w", err)
		}
		log.Info("report written", "dir", dir)
	}
	log.Info("done", "label_config", labelConfig)
	return ctx.Err()
}

// labelAll labels posts with a worker pool, writing labels and request logs every 100
// posts. It returns how many posts' batches failed (they stay unlabeled for a retry).
func labelAll(ctx context.Context, log *slog.Logger, lb *labeler.Labeler, store *labeler.Store,
	taxVersion, labelConfig string, cfg labeler.Config, posts []labeler.Post) (int64, error) {

	batches := make(chan []labeler.Post)
	results := make(chan []labeler.Label)
	var failed atomic.Int64

	var wg sync.WaitGroup
	for w := 0; w < cfg.Concurrency; w++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for b := range batches {
				labels, err := lb.LabelBatch(ctx, b)
				if err != nil {
					if ctx.Err() == nil {
						failed.Add(int64(len(b)))
						log.Warn("batch failed", "err", err, "first_uri", b[0].URI)
					}
					continue
				}
				results <- labels
			}
		}()
	}
	go func() {
		defer close(batches)
		for i := 0; i < len(posts); i += cfg.BatchSize {
			select {
			case batches <- posts[i:min(i+cfg.BatchSize, len(posts))]:
			case <-ctx.Done():
				return
			}
		}
	}()
	go func() { wg.Wait(); close(results) }()

	var pending []labeler.Label
	done, lastLogged := 0, 0
	start := time.Now()
	flush := func() error {
		wctx, cancel := labeler.WithTimeout(ctx)
		defer cancel()
		if err := store.WriteLabels(wctx, taxVersion, labelConfig, cfg.Source, cfg.BatchSize, pending); err != nil {
			return fmt.Errorf("write labels: %w", err)
		}
		if err := store.WriteRequests(wctx, lb.DrainRequests()); err != nil {
			return fmt.Errorf("write requests: %w", err)
		}
		done += len(pending)
		pending = pending[:0]
		if done-lastLogged >= 1000 || done == len(posts) {
			lastLogged = done
			elapsed := time.Since(start)
			rate := float64(done) / elapsed.Minutes()
			eta := time.Duration(float64(len(posts)-done)/max(rate, 1e-9)) * time.Minute
			log.Info("progress", "labeled", done, "failed", failed.Load(), "of", len(posts),
				"per_min", int(rate), "elapsed", elapsed.Round(time.Second).String(),
				"eta", eta.Round(time.Minute).String(), "finish_utc", time.Now().Add(eta).UTC().Format("15:04"))
		}
		return nil
	}
	for labels := range results {
		pending = append(pending, labels...)
		if len(pending) >= 100 {
			if err := flush(); err != nil {
				return failed.Load(), err
			}
		}
	}
	if err := flush(); err != nil {
		return failed.Load(), err
	}
	return failed.Load(), ctx.Err()
}

// retryPolicy retries rate limits and server errors patiently, honoring Retry-After.
func retryPolicy() typesafe.RetryPolicy {
	p := typesafe.DefaultRetryPolicy()
	p.MaxRetries = 6
	p.BackoffMax = 30 * time.Second
	p.TotalBudget = 5 * time.Minute
	return p
}

func envOr(k, def string) string {
	if v := os.Getenv(k); v != "" {
		return v
	}
	return def
}
