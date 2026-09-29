package ingest

import (
	"context"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/haileyok/topic-feed/internal/chdb"
)

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
	if err := insert(ctx, w.Conn, "post_refs", rows.PostRefs); err != nil {
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
	if err := chdb.Insert(ctx, conn, table, rows); err != nil {
		return err
	}
	metricRowsWritten.WithLabelValues(table).Add(float64(len(rows)))
	return nil
}
