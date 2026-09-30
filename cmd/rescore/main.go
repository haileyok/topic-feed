// Command rescore re-classifies posts already in post_pipeline with the classifier
// service's current model, e.g. after switching models, so feeds see the new model's
// scores for their whole window and not only for posts that arrive afterwards.
//
//	rescore -from-model v3-blend -before 2026-09-30T18:00:00Z
//
// It reuses the stored labels and image text (no image fetches, no LLM calls) and is
// safe to rerun: only rows still on -from-model are picked. Environment: CLASSIFIER_URL
// (default http://127.0.0.1:8700) and the CLICKHOUSE_* variables read by chdb.FromEnv.
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"slices"
	"syscall"
	"time"

	"github.com/haileyok/topic-feed/internal/chdb"
	"github.com/haileyok/topic-feed/internal/pipeline"
	"github.com/haileyok/topic-feed/internal/postdoc"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, nil))
	if err := run(log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("rescore failed", "err", err)
		os.Exit(1)
	}
}

func run(log *slog.Logger) error {
	fromModel := flag.String("from-model", "", "re-classify rows this model classified (required)")
	beforeS := flag.String("before", "", "only posts indexed before this time, RFC 3339 (required)")
	window := flag.Duration("window", 10*time.Minute, "posts per step, by indexed_at")
	flag.Parse()
	if *fromModel == "" || *beforeS == "" {
		return errors.New("-from-model and -before are required")
	}
	before, err := time.Parse(time.RFC3339, *beforeS)
	if err != nil {
		return fmt.Errorf("-before: %w", err)
	}
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	conn, err := chdb.Open(ctx, chdb.FromEnv())
	if err != nil {
		return err
	}
	defer conn.Close()
	url := os.Getenv("CLASSIFIER_URL")
	if url == "" {
		url = "http://127.0.0.1:8700"
	}
	c := &pipeline.Classifier{URL: url, Client: &http.Client{Timeout: 2 * time.Minute}}
	hctx, cancel := context.WithTimeout(ctx, 10*time.Second)
	h, err := c.Health(hctx)
	cancel()
	if err != nil {
		return fmt.Errorf("classifier: %w", err)
	}
	if h.Model == *fromModel {
		return fmt.Errorf("the classifier is still serving %s; switch it to the new model first", h.Model)
	}
	if !slices.Contains(postdoc.Versions, h.PostdocVersion) {
		return fmt.Errorf("classifier model %s expects post documents %s, this build renders %v", h.Model, h.PostdocVersion, postdoc.Versions)
	}
	p := &pipeline.Pipeline{Conn: conn, Classifier: c, PostdocVersion: h.PostdocVersion, Log: log}
	log.Info("classifier", "model", h.Model, "postdoc", h.PostdocVersion)
	return p.Rescore(ctx, *fromModel, before, *window)
}
