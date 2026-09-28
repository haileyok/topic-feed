// Command labeler labels posts with hosted Jev (plan §10) and writes jev_labels and
// jev_requests.
//
//	labeler -taxonomy taxonomy/draft1.yaml -limit 2500 -source window
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
	"github.com/haileyok/topic-feed/internal/taxonomy"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("labeler failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	var (
		taxPath     = flag.String("taxonomy", "taxonomy/draft1.yaml", "taxonomy YAML file")
		limit       = flag.Int("limit", 2500, "posts to label, then stop")
		source      = flag.String("source", "window", "jev_labels.source value")
		batchSize   = flag.Int("batch", 1, "posts per Jev request")
		topK        = flag.Int("topk", 3, "broad topics per post that get a subtopic question")
		minBroadP   = flag.Float64("min-broad-p", 0.05, "skip broad candidates below this probability")
		concurrency = flag.Int("concurrency", 16, "batches in flight")
		maxRPM      = flag.Int("max-rpm", 500, "Jev requests per minute (our share of the shared account)")
		maxTPS      = flag.Int("max-tps", 100_000, "estimated Jev input tokens per second")
	)
	flag.Parse()

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	tax, err := taxonomy.Load(*taxPath)
	if err != nil {
		return err
	}
	cfg := labeler.Config{
		Model: envOr("JEV_MODEL", "jev-1.13.0"), BatchSize: *batchSize, TopK: *topK, MinBroadP: *minBroadP,
		Source: *source, Concurrency: *concurrency, MaxRPM: *maxRPM, MaxTPS: *maxTPS,
	}
	labelConfig := labeler.LabelConfig(tax, cfg)

	client, err := typesafe.NewClient(
		typesafe.WithAPIKey(os.Getenv("TYPESAFE_API_KEY")),
		typesafe.WithBaseURL(envOr("TYPESAFE_BASE_URL", "https://agw.noclues.net")),
		typesafe.WithModel(cfg.Model),
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

	posts, err := store.SelectSpread(ctx, tax.Version, labelConfig, *limit)
	if err != nil {
		return err
	}
	log.Info("labeling", "posts", len(posts), "taxonomy", tax.Version, "label_config", labelConfig,
		"batch", cfg.BatchSize, "topk", cfg.TopK, "model", cfg.Model)

	lb := labeler.New(cfg, tax, client, log)
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

	// Write labels and request logs every 100 posts, and at the end.
	var pending []labeler.Label
	done := 0
	start := time.Now()
	flush := func() error {
		wctx, cancel := labeler.WithTimeout(ctx)
		defer cancel()
		if err := store.WriteLabels(wctx, tax.Version, labelConfig, cfg.Source, cfg.BatchSize, pending); err != nil {
			return fmt.Errorf("write labels: %w", err)
		}
		if err := store.WriteRequests(wctx, lb.DrainRequests()); err != nil {
			return fmt.Errorf("write requests: %w", err)
		}
		done += len(pending)
		pending = pending[:0]
		log.Info("progress", "labeled", done, "failed", failed.Load(), "of", len(posts), "elapsed", time.Since(start).Round(time.Second).String())
		return nil
	}
	for labels := range results {
		pending = append(pending, labels...)
		if len(pending) >= 100 {
			if err := flush(); err != nil {
				return err
			}
		}
	}
	if err := flush(); err != nil {
		return err
	}
	log.Info("done", "labeled", done, "failed", failed.Load(), "label_config", labelConfig)
	return ctx.Err()
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
