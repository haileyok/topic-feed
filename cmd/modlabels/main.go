// Command modlabels follows the Bluesky moderation service's label stream and writes
// every label and label removal to ClickHouse (table mod_labels).
//
// Configuration comes from environment variables:
//
//	MODLABELS_SERVICE     labeler endpoint, default https://mod.bsky.app
//	MODLABELS_CONSUMER    row key in ingest_cursor, default modlabels:<service host>
//	CLICKHOUSE_ADDR       default localhost:9000
//	CLICKHOUSE_DB         default topicfeed
//	CLICKHOUSE_USER       default topicfeed
//	CLICKHOUSE_PASSWORD   required
//	METRICS_ADDR          default :9102
//
// Flags:
//
//	-dump N   print the first N decoded frames from the live stream and exit (no writes)
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/coder/websocket"
	"github.com/prometheus/client_golang/prometheus/promhttp"

	"github.com/haileyok/topic-feed/internal/chdb"
	"github.com/haileyok/topic-feed/internal/modlabels"
)

func main() {
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: slog.LevelInfo}))
	dump := flag.Int("dump", 0, "print the first N frames from the live stream and exit")
	flag.Parse()
	if err := run(log, *dump); err != nil && !errors.Is(err, context.Canceled) {
		log.Error("modlabels stopped", "err", err)
		os.Exit(1)
	}
	log.Info("modlabels stopped")
}

func run(log *slog.Logger, dump int) error {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	service := env("MODLABELS_SERVICE", "https://mod.bsky.app")
	if dump > 0 {
		return dumpFrames(ctx, service, dump)
	}
	if os.Getenv("CLICKHOUSE_PASSWORD") == "" {
		return errors.New("CLICKHOUSE_PASSWORD must be set")
	}
	u, err := url.Parse(service)
	if err != nil {
		return err
	}

	go func() {
		mux := http.NewServeMux()
		mux.Handle("/metrics", promhttp.Handler())
		mux.HandleFunc("/healthz", func(w http.ResponseWriter, _ *http.Request) { w.Write([]byte("ok")) })
		if err := http.ListenAndServe(env("METRICS_ADDR", ":9102"), mux); err != nil {
			log.Error("metrics server", "err", err)
		}
	}()

	conn, err := chdb.Open(ctx, chdb.FromEnv())
	if err != nil {
		return err
	}
	defer conn.Close()

	s := &modlabels.Stream{
		Cfg: modlabels.Config{
			Service:    service,
			Consumer:   env("MODLABELS_CONSUMER", "modlabels:"+u.Host),
			FlushEvery: time.Second,
			FlushRows:  2000,
			IdleAfter:  5 * time.Minute,
		},
		Conn: conn,
		Log:  log,
	}
	return s.Run(ctx)
}

// dumpFrames prints decoded frames from the live stream, to check the format.
func dumpFrames(ctx context.Context, service string, n int) error {
	u, _ := url.Parse(service + "/xrpc/com.atproto.label.subscribeLabels")
	u.Scheme = "wss"
	ws, _, err := websocket.Dial(ctx, u.String(), nil)
	if err != nil {
		return err
	}
	defer ws.CloseNow()
	ws.SetReadLimit(8 << 20)
	for i := 0; i < n; i++ {
		rctx, cancel := context.WithTimeout(ctx, 2*time.Minute)
		_, data, err := ws.Read(rctx)
		cancel()
		if err != nil {
			return err
		}
		f, err := modlabels.DecodeFrame(data)
		if err != nil {
			fmt.Printf("frame %d: undecodable: %v\n", i, err)
			continue
		}
		fmt.Printf("frame %d: %s seq=%d info=%q err=%q\n", i, f.Kind, f.Seq, f.Info, f.Err)
		for _, l := range f.Labels {
			fmt.Printf("  src=%s uri=%s val=%s neg=%v cts=%s exp=%s cid=%s sig=%dB\n", l.Src, l.URI, l.Val, l.Neg, l.Cts, l.Exp, l.CID, len(l.Sig))
		}
	}
	return nil
}

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}
