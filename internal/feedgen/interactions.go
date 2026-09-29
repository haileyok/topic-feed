package feedgen

import (
	"context"
	"log/slog"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"

	"github.com/haileyok/topic-feed/internal/chdb"
)

// InteractionRow is one row of feed_interactions.
type InteractionRow struct {
	ReceivedAt  time.Time `ch:"received_at"`
	ViewerDID   string    `ch:"viewer_did"`
	Feed        string    `ch:"feed"`
	Item        string    `ch:"item"`
	Event       string    `ch:"event"`
	FeedContext string    `ch:"feed_context"`
	ReqID       string    `ch:"req_id"`
}

// InteractionSink accepts interactions for storage. *InteractionWriter implements it.
type InteractionSink interface {
	// Add queues rows and returns how many were dropped because the queue was full.
	Add(rows []InteractionRow) (dropped int)
}

// InteractionWriter batches interactions into ClickHouse in the background, so requests
// never wait on the database.
type InteractionWriter struct {
	Conn       driver.Conn
	Log        *slog.Logger
	FlushEvery time.Duration
	FlushRows  int

	queue chan InteractionRow
	done  chan struct{}
}

// NewInteractionWriter makes a writer with room for queueSize rows. Call Run.
func NewInteractionWriter(conn driver.Conn, log *slog.Logger, queueSize int) *InteractionWriter {
	return &InteractionWriter{Conn: conn, Log: log, FlushEvery: 2 * time.Second, FlushRows: 5000,
		queue: make(chan InteractionRow, queueSize), done: make(chan struct{})}
}

func (w *InteractionWriter) Add(rows []InteractionRow) int {
	dropped := 0
	for _, r := range rows {
		select {
		case w.queue <- r:
		default:
			dropped++
		}
	}
	if dropped > 0 {
		metricInteractionsDropped.Add(float64(dropped))
	}
	return dropped
}

// Run writes queued rows until ctx ends, then writes what's left and returns. Wait
// returns once it has.
func (w *InteractionWriter) Run(ctx context.Context) {
	defer close(w.done)
	t := time.NewTicker(w.FlushEvery)
	defer t.Stop()
	var buf []InteractionRow
	flush := func(ctx context.Context) {
		if len(buf) == 0 {
			return
		}
		if err := chdb.Insert(ctx, w.Conn, "feed_interactions", buf); err != nil {
			metricInteractionWriteErrors.Inc()
			w.Log.Error("write interactions", "rows", len(buf), "err", err)
			if len(buf) > 100_000 { // ClickHouse is down for a while: don't grow without bound
				metricInteractionsDropped.Add(float64(len(buf)))
				buf = buf[:0]
			}
			return
		}
		metricInteractionsWritten.Add(float64(len(buf)))
		buf = buf[:0]
	}
	for {
		select {
		case r := <-w.queue:
			buf = append(buf, r)
			if len(buf) >= w.FlushRows {
				flush(ctx)
			}
		case <-t.C:
			flush(ctx)
		case <-ctx.Done():
		drain:
			for {
				select {
				case r := <-w.queue:
					buf = append(buf, r)
				default:
					break drain
				}
			}
			fctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			flush(fctx)
			cancel()
			return
		}
	}
}

// Wait blocks until Run has returned.
func (w *InteractionWriter) Wait() { <-w.done }
