// Command postmap serves the post map: every post on a 2D map, in segments and areas an LLM has named. It reads
// the snapshots postclusters/build_snapshot.py writes and needs no database.
//
// Settings (flags or environment; the environment wins when a flag is left at its default):
//
//	POSTMAP_DIR   directory holding the snapshots and the "latest" link to the newest (postclusters/data/snapshot)
//	HTTP_ADDR     listen address (127.0.0.1:8740)
//	LOG_LEVEL     debug, info, warn or error (info)
//
// A new snapshot is picked up within a minute of "latest" being repointed; nothing restarts. The service shows the
// text of public posts and has no access control, so it listens on the loopback address unless told otherwise.
package main

import (
	"context"
	"errors"
	"flag"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"github.com/haileyok/topic-feed/internal/postmap"
)

func env(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

// healthcheck asks the service listening on addr for /healthz and returns the exit code.
func healthcheck(addr string) int {
	host, port, err := net.SplitHostPort(addr)
	if err != nil {
		return 2
	}
	if host == "" || host == "0.0.0.0" || host == "::" {
		host = "127.0.0.1"
	}
	c := http.Client{Timeout: 3 * time.Second}
	resp, err := c.Get("http://" + net.JoinHostPort(host, port) + "/healthz")
	if err != nil {
		return 1
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		return 1
	}
	return 0
}

func main() {
	dir := flag.String("dir", env("POSTMAP_DIR", "postclusters/data/snapshot"), "snapshot directory")
	addr := flag.String("addr", env("HTTP_ADDR", "127.0.0.1:8740"), "listen address")
	health := flag.Bool("healthcheck", false, "ask the running service on -addr whether it has a map loaded, and exit 0 if so (for container health checks)")
	flag.Parse()
	if *health {
		os.Exit(healthcheck(*addr))
	}

	var level slog.Level
	if err := level.UnmarshalText([]byte(env("LOG_LEVEL", "info"))); err != nil {
		level = slog.LevelInfo
	}
	log := slog.New(slog.NewJSONHandler(os.Stdout, &slog.HandlerOptions{Level: level}))

	s := &postmap.Server{Dir: *dir, Log: log}
	if err := s.Load(); err != nil {
		// Keep running: the first map may simply not be built yet, and Watch will pick it up.
		log.Warn("no map loaded", "err", err)
	}
	stop := make(chan struct{})
	go s.Watch(time.Minute, stop)

	srv := &http.Server{
		Addr:              *addr,
		Handler:           s.Handler(),
		ReadHeaderTimeout: 10 * time.Second,
		ReadTimeout:       30 * time.Second,
		IdleTimeout:       2 * time.Minute,
	}
	go func() {
		sig := make(chan os.Signal, 1)
		signal.Notify(sig, syscall.SIGINT, syscall.SIGTERM)
		<-sig
		close(stop)
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
		defer cancel()
		srv.Shutdown(ctx)
	}()
	log.Info("serving", "addr", *addr, "dir", *dir)
	if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
		log.Error("server", "err", err)
		os.Exit(1)
	}
}
