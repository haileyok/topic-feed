// Command pipeline processes live posts shortly after ingest: it applies the label policy
// (config/label_policy.yaml), downloads each post's first pictures, has the classifier service
// (trainer/serve.py) look at the post's text and pictures, and writes post_pipeline.
//
// Configuration comes from environment variables:
//
//	PIPELINE_DELAY_SECONDS   process posts once they are this old, default 10
//	PIPELINE_CONSUMER        ingest_cursor row for the pipeline's position, default pipeline
//	LABEL_POLICY             default config/label_policy.yaml
//	IMAGE_RETRY              "off" disables retrying posts whose pictures didn't download (image_retry_queue), default on
//	IMAGE_RETRY_EVERY_SECONDS  how often to queue failures and run due retries, default 120
//	IMAGE_RETRY_WINDOW_HOURS   how far back failures are queued, default 24 (what feeds show)
//	IMAGE_RETRY_MAX_ATTEMPTS   retries before giving up on a post, default 5
//	IMAGE_RETRY_WORKERS        posts retried at once, default 4
//	CLASSIFIER_URL           classifier service, default http://127.0.0.1:8700 ("none" disables)
//	CLICKHOUSE_*             see internal/chdb
//	METRICS_ADDR             default :9103
package main

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/haileyok/topic-feed/internal/chdb"
	"github.com/haileyok/topic-feed/internal/labelpolicy"
	"github.com/haileyok/topic-feed/internal/pipeline"
	"github.com/haileyok/topic-feed/internal/postdoc"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("pipeline stopped", "err", err)
		os.Exit(1)
	}
	log.Info("pipeline stopped")
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	policy, err := labelpolicy.Load(env("LABEL_POLICY", "config/label_policy.yaml"))
	if err != nil {
		return err
	}
	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
		if err := http.ListenAndServe(env("METRICS_ADDR", ":9103"), mux); err != nil {
			log.Error("metrics server", "err", err)
		}
	}()

	conn, err := chdb.Open(ctx, chdb.FromEnv())
	if err != nil {
		return err
	}
	defer conn.Close()

	httpClient := &http.Client{Timeout: 90 * time.Second}
	p := &pipeline.Pipeline{
		Cfg: pipeline.Config{
			Delay:      time.Duration(envInt("PIPELINE_DELAY_SECONDS", 10)) * time.Second,
			Consumer:   env("PIPELINE_CONSUMER", "pipeline"),
			BatchLimit: 2000,
			Poll:       time.Second,
		},
		Conn:   conn,
		Policy: policy,
		HTTP:   httpClient,
		Log:    log,
	}
	if u := env("CLASSIFIER_URL", "http://127.0.0.1:8700"); u != "" && u != "none" {
		p.Classifier = &pipeline.Classifier{URL: u, Client: httpClient}
		h, err := waitForClassifier(ctx, p.Classifier, log)
		if err != nil {
			return err
		}
		if h.PostdocVersion != postdoc.Version {
			return fmt.Errorf("classifier model %s expects post documents %s, this build renders %s", h.Model, h.PostdocVersion, postdoc.Version)
		}
		p.Cfg.MaxPictures = h.MaxImages
		log.Info("classifier ready", "url", u, "model", h.Model, "taxonomy", h.TaxonomyVersion, "postdoc", h.PostdocVersion,
			"device", h.Device, "pictures_per_post", h.MaxImages)
	}
	if p.Classifier != nil && env("IMAGE_RETRY", "on") != "off" {
		cfg := pipeline.DefaultRetry
		cfg.Every = time.Duration(envInt("IMAGE_RETRY_EVERY_SECONDS", int(cfg.Every.Seconds()))) * time.Second
		cfg.Window = time.Duration(envInt("IMAGE_RETRY_WINDOW_HOURS", int(cfg.Window.Hours()))) * time.Hour
		cfg.MaxAttempts = envInt("IMAGE_RETRY_MAX_ATTEMPTS", cfg.MaxAttempts)
		cfg.Workers = envInt("IMAGE_RETRY_WORKERS", cfg.Workers)
		go p.RetryPictures(ctx, cfg)
	}
	return p.Run(ctx)
}

// waitForClassifier waits for the classifier service to answer, since it may start
// after the pipeline (it runs on the host under systemd).
func waitForClassifier(ctx context.Context, c *pipeline.Classifier, log *slog.Logger) (pipeline.Health, error) {
	for attempt := 0; ; attempt++ {
		hctx, cancel := context.WithTimeout(ctx, 5*time.Second)
		h, err := c.Health(hctx)
		cancel()
		if err == nil {
			return h, nil
		}
		if attempt%6 == 0 {
			log.Warn("waiting for the classifier service", "url", c.URL, "err", err)
		}
		select {
		case <-ctx.Done():
			return h, ctx.Err()
		case <-time.After(5 * time.Second):
		}
	}
}

func env(key, def string) string {
	if v, ok := os.LookupEnv(key); ok {
		return v
	}
	return def
}

func envInt(key string, def int) int {
	if v, err := strconv.Atoi(os.Getenv(key)); err == nil {
		return v
	}
	return def
}
