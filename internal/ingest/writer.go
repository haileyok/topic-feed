package ingest

import (
	"context"
	"fmt"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
)

// ClickHouseConfig holds connection settings.
type ClickHouseConfig struct {
	Addr     string // host:port of the native protocol
	Database string
	User     string
	Password string
}

// OpenClickHouse opens and pings a native-protocol connection.
func OpenClickHouse(ctx context.Context, cfg ClickHouseConfig) (driver.Conn, error) {
	conn, err := clickhouse.Open(&clickhouse.Options{
		Addr: []string{cfg.Addr},
		Auth: clickhouse.Auth{Database: cfg.Database, Username: cfg.User, Password: cfg.Password},
		Compression: &clickhouse.Compression{
			Method: clickhouse.CompressionLZ4,
		},
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

// Writer inserts parsed rows and saves the stream position.
type Writer struct {
	Conn     driver.Conn
	Consumer string // row key in ingest_cursor
}

// Write inserts every table's rows. Tables are written one after another; a failure
// part-way leaves earlier tables written, which is harmless because the caller
// retries the whole flush and the tables collapse duplicates.
func (w *Writer) Write(ctx context.Context, rows *Rows) error {
	if err := insert(ctx, w.Conn, "post_texts", rows.PostTexts); err != nil {
		return err
	}
	if err := insert(ctx, w.Conn, "posts", rows.Posts); err != nil {
		return err
	}
	if err := insert(ctx, w.Conn, "likes", rows.Likes); err != nil {
		return err
	}
	if err := insert(ctx, w.Conn, "reposts", rows.Reposts); err != nil {
		return err
	}
	if err := insert(ctx, w.Conn, "deletions", rows.Deletions); err != nil {
		return err
	}
	return insert(ctx, w.Conn, "account_status", rows.Accounts)
}

// SaveCursor records the last sequence number whose rows have all been written.
func (w *Writer) SaveCursor(ctx context.Context, seq uint64) error {
	return w.Conn.Exec(ctx, "INSERT INTO ingest_cursor (consumer, cursor, updated_at) VALUES (?, ?, ?)",
		w.Consumer, seq, time.Now().UTC())
}

// LoadCursor returns the saved sequence number, or ok=false if there is none.
func (w *Writer) LoadCursor(ctx context.Context) (seq uint64, ok bool, err error) {
	rows, err := w.Conn.Query(ctx, "SELECT argMax(cursor, updated_at) FROM ingest_cursor WHERE consumer = ? GROUP BY consumer", w.Consumer)
	if err != nil {
		return 0, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, false, rows.Err()
	}
	if err := rows.Scan(&seq); err != nil {
		return 0, false, err
	}
	return seq, true, nil
}

func insert[T any](ctx context.Context, conn driver.Conn, table string, rows []T) error {
	if len(rows) == 0 {
		return nil
	}
	batch, err := conn.PrepareBatch(ctx, "INSERT INTO "+table)
	if err != nil {
		return fmt.Errorf("prepare %s: %w", table, err)
	}
	for i := range rows {
		if err := batch.AppendStruct(&rows[i]); err != nil {
			_ = batch.Abort()
			return fmt.Errorf("append %s: %w", table, err)
		}
	}
	if err := batch.Send(); err != nil {
		return fmt.Errorf("send %s: %w", table, err)
	}
	metricRowsWritten.WithLabelValues(table).Add(float64(len(rows)))
	return nil
}
