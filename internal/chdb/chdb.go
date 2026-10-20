// Package chdb opens ClickHouse connections for the Go services.
package chdb

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// Config holds connection settings.
type Config struct {
	Addr     string // host:port of the native protocol
	Database string
	User     string
	Password string
	// App names the service in ClickHouse's query log (log_comment) and so in what it costs; the
	// program's name when empty. Set it with For: in the containers every program is called "app".
	App string
}

// For is the config with App set to the service's name.
func (c Config) For(app string) Config {
	c.App = app
	return c
}

// app is the service's name for the queries it makes (see observe.go); Open sets it.
var app = filepath.Base(os.Args[0])

// FromEnv reads CLICKHOUSE_ADDR, CLICKHOUSE_DB, CLICKHOUSE_USER, and
// CLICKHOUSE_PASSWORD, with local defaults for all but the password.
func FromEnv() Config {
	get := func(k, def string) string {
		if v := os.Getenv(k); v != "" {
			return v
		}
		return def
	}
	return Config{
		Addr:     get("CLICKHOUSE_ADDR", "localhost:9000"),
		Database: get("CLICKHOUSE_DB", "topicfeed"),
		User:     get("CLICKHOUSE_USER", "topicfeed"),
		Password: os.Getenv("CLICKHOUSE_PASSWORD"),
	}
}

// Open opens and pings a native-protocol connection. Every query made on it is measured and named
// after the service and the operation (observe.go).
func Open(ctx context.Context, cfg Config) (driver.Conn, error) {
	if cfg.App != "" {
		app = cfg.App
	}
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:            []string{cfg.Addr},
		Auth:            clickhouse.Auth{Database: cfg.Database, Username: cfg.User, Password: cfg.Password},
		Compression:     &clickhouse.Compression{Method: clickhouse.CompressionLZ4},
		DialTimeout:     10 * time.Second,
		MaxOpenConns:    8,
		ConnMaxLifetime: time.Hour,
		// The name of queries no Op names; an Op's own log_comment replaces it.
		Settings: clickhouse.Settings{"log_comment": app + "/other"},
	})
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping clickhouse: %w", err)
	}
	return observed{conn}, nil
}
