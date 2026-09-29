// Command pipeline processes live posts shortly after ingest: it applies the label policy
// (config/label_policy.yaml), finds text in images without alt text (tesseract first,
// then an LLM description within a daily budget), and writes post_pipeline.
//
// Configuration comes from environment variables:
//
//	PIPELINE_DELAY_SECONDS   process posts once they are this old, default 10
//	PIPELINE_CONSUMER        ingest_cursor row for the pipeline's position, default pipeline
//	INGEST_CONSUMER          ingest's row (never pass what it has written), default ingest
//	LABEL_POLICY             default config/label_policy.yaml
//	TESSERACT                tesseract executable, default tesseract
//	OCR_WORKERS              tesseract processes at once, default 4
//	OCR_MIN_WORDS            confident words needed to use tesseract's text, default 7
//	LLM_MODEL                vision model for descriptions, default gpt-6-luna:api ("" disables)
//	LLM_WORKERS              descriptions at once, default 16
//	LLM_DAILY_BUDGET_USD     list-price cap per UTC day, default 10
//	TYPESAFE_API_KEY         AI gateway key (required when LLM_MODEL is set)
//	TYPESAFE_BASE_URL        AI gateway, default https://agw.noclues.net
//	CLICKHOUSE_*             see internal/chdb
//	METRICS_ADDR             default :9103
package main

import (
	"context"
	"errors"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"strconv"
	"syscall"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/haileyok/topic-feed/internal/chdb"
	"github.com/haileyok/topic-feed/internal/pipeline"
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

	policy, err := pipeline.LoadPolicy(env("LABEL_POLICY", "config/label_policy.yaml"))
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

	httpClient := &http.Client{Timeout: 60 * time.Second}
	p := &pipeline.Pipeline{
		Cfg: pipeline.Config{
			Delay:          time.Duration(envInt("PIPELINE_DELAY_SECONDS", 10)) * time.Second,
			Consumer:       env("PIPELINE_CONSUMER", "pipeline"),
			IngestConsumer: env("INGEST_CONSUMER", "ingest"),
			BatchLimit:     2000,
			MinOCRWords:    envInt("OCR_MIN_WORDS", 7),
			MaxMedia:       4,
			Poll:           time.Second,
		},
		Conn:   conn,
		Policy: policy,
		OCR:    &pipeline.OCR{Binary: env("TESSERACT", "tesseract"), MinConf: 70, Workers: envInt("OCR_WORKERS", 4)},
		HTTP:   httpClient,
		Log:    log,
	}
	if model := env("LLM_MODEL", "gpt-6-luna:api"); model != "" && model != "none" {
		key := os.Getenv("TYPESAFE_API_KEY")
		if key == "" {
			return errors.New("TYPESAFE_API_KEY must be set for image descriptions (or set LLM_MODEL=none)")
		}
		spent, err := spentToday(ctx, conn)
		if err != nil {
			return err
		}
		budget, _ := strconv.ParseFloat(env("LLM_DAILY_BUDGET_USD", "10"), 64)
		p.Describer = &pipeline.Describer{
			Endpoint: env("TYPESAFE_BASE_URL", "https://agw.noclues.net"), APIKey: key, Model: model, Client: httpClient,
			// gpt-6-luna list prices per million tokens (OpenAI API docs, 2026-09).
			Prices:  pipeline.Prices{Input: 0.10, CachedInput: 0.01, Output: 0.50},
			Budget:  pipeline.NewBudget(budget, spent),
			Workers: envInt("LLM_WORKERS", 16),
		}
		log.Info("image descriptions on", "model", model, "budget_usd_per_day", budget, "spent_today_usd", spent)
	}
	return p.Run(ctx)
}

// spentToday is the list-price LLM spending already recorded today (UTC), so a restart
// doesn't reset the daily budget.
func spentToday(ctx context.Context, conn driver.Conn) (float64, error) {
	var v float64
	err := conn.QueryRow(ctx, "SELECT sum(luna_cost_usd) FROM post_pipeline WHERE processed_at >= toStartOfDay(now64(3))").Scan(&v)
	return v, err
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
