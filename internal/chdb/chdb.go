// Package chdb opens ClickHouse connections for the Go services.
package chdb

import (
	"context"
	"fmt"
	"os"
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
}

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

// Open opens and pings a native-protocol connection.
func Open(ctx context.Context, cfg Config) (driver.Conn, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr:            []string{cfg.Addr},
		Auth:            clickhouse.Auth{Database: cfg.Database, Username: cfg.User, Password: cfg.Password},
		Compression:     &clickhouse.Compression{Method: clickhouse.CompressionLZ4},
		DialTimeout:     10 * time.Second,
		MaxOpenConns:    8,
		ConnMaxLifetime: time.Hour,
	})
	if err != nil {
		return nil, err
	}
	if err := conn.Ping(ctx); err != nil {
		return nil, fmt.Errorf("ping clickhouse: %w", err)
	}
	return conn, nil
}
