// Command rescore re-classifies posts already in post_pipeline with the classifier
// service's current model, e.g. after switching models, so feeds see the new model's
// scores for their whole window and not only for posts that arrive afterwards.
//
//	rescore -from-model v4 -after 2026-10-01T00:00:00Z -before 2026-10-03T18:00:00Z
//
// It reuses the stored labels and label policy decisions, downloads each post's first
// pictures again (the model looks at them; a post whose pictures are gone is classified
// without them), and is safe to rerun: only rows still on -from-model are picked.
// Environment: CLASSIFIER_URL (default http://127.0.0.1:8700) and the CLICKHOUSE_*
// variables read by chdb.FromEnv.
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

// noModel is the -from-model value for rows the pipeline never classified (stored with an empty model).
const noModel = "none"

// storedModel is the model name to look for in post_pipeline for a -from-model value.
func storedModel(flagValue string) string {
	if flagValue == noModel {
		return ""
	}
	return flagValue
}

func run(log *slog.Logger) error {
	fromModel := flag.String("from-model", "", "re-classify rows this model classified, or \"none\" for rows that were never classified (required)")
	afterS := flag.String("after", "", "only posts indexed at or after this time, RFC 3339 (default: from the first one; feeds only show the last 24h)")
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
	var after time.Time
	if *afterS != "" {
		if after, err = time.Parse(time.RFC3339, *afterS); err != nil {
			return fmt.Errorf("-after: %w", err)
		}
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
	if h.Model == storedModel(*fromModel) {
		return fmt.Errorf("the classifier is still serving %s; switch it to the new model first", h.Model)
	}
	if h.PostdocVersion != postdoc.Version {
		return fmt.Errorf("classifier model %s expects post documents %s, this build renders %s", h.Model, h.PostdocVersion, postdoc.Version)
	}
	p := &pipeline.Pipeline{Cfg: pipeline.Config{MaxPictures: h.MaxImages}, Conn: conn, Classifier: c,
		HTTP: &http.Client{Timeout: 60 * time.Second}, Log: log}
	log.Info("classifier", "model", h.Model, "postdoc", h.PostdocVersion, "pictures_per_post", h.MaxImages)
	return p.Rescore(ctx, storedModel(*fromModel), after, before, *window)
}
