// Command ingest consumes Jetstream and writes to ClickHouse (plan §9).
//
// Configuration comes from environment variables:
//
//	JETSTREAM_HOST        default jetstream.us-east.bsky.network
//	JETSTREAM_API_KEY     required (archive replay)
//	CLICKHOUSE_ADDR       default localhost:9000
//	CLICKHOUSE_DB         default topicfeed
//	CLICKHOUSE_USER       default topicfeed
//	CLICKHOUSE_PASSWORD   required
//	INGEST_CONSUMER       row key in ingest_cursor, default "ingest"
//	INGEST_START_HOURS    first-run start, hours back, default 72
//	APPVIEW_URL           default https://public.api.bsky.app
//	TEXT_CACHE_SIZE       post texts kept in memory for quote lookups, default 3000000
//	METRICS_ADDR          default :9101
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

	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/haileyok/topic-feed/internal/ingest"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	if err := run(log); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("ingest stopped", "err", err)
		os.Exit(1)
	}
	log.Info("ingest stopped")
}

func run(log *slog.Logger) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	apiKey := os.Getenv("JETSTREAM_API_KEY")
	password := os.Getenv("CLICKHOUSE_PASSWORD")
	if apiKey == "" || password == "" {
		return errors.New("JETSTREAM_API_KEY and CLICKHOUSE_PASSWORD must be set")
	}

	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
		addr := env("METRICS_ADDR", ":9101")
		if err := http.ListenAndServe(addr, mux); err != nil {
			log.Error("metrics server", "err", err)
		}
	}()

	conn, err := ingest.OpenClickHouse(ctx, ingest.ClickHouseConfig{
		Addr:     env("CLICKHOUSE_ADDR", "localhost:9000"),
		Database: env("CLICKHOUSE_DB", "topicfeed"),
		User:     env("CLICKHOUSE_USER", "topicfeed"),
		Password: password,
	})
	if err != nil {
		return err
	}
	defer conn.Close()

	log.Info("loading language detector")
	cache := ingest.NewTextCache(envInt("TEXT_CACHE_SIZE", 3_000_000))
	in := &ingest.Ingester{
		Cfg: ingest.Config{
			JetstreamHost:   env("JETSTREAM_HOST", "jetstream.us-east.bsky.network"),
			JetstreamAPIKey: apiKey,
			StartBack:       time.Duration(envInt("INGEST_START_HOURS", 72)) * time.Hour,
			FlushEvery:      time.Second,
			FlushRows:       20_000,
		},
		Parser: &ingest.Parser{Lang: ingest.NewLangChecker(), Texts: cache},
		Writer: &ingest.Writer{Conn: conn, Consumer: env("INGEST_CONSUMER", "ingest")},
		Quotes: ingest.NewQuoteResolver(cache, conn, env("APPVIEW_URL", "https://public.api.bsky.app"), log.With("component", "quotes")),
		Log:    log,
	}
	return in.Run(ctx)
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
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
