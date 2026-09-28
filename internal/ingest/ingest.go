// Package ingest consumes Jetstream (archive replay, then live) and writes posts,
// likes, reposts, deletions, and account status to ClickHouse (plan §9).
package ingest

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/bluesky-social/jetstream"
)

// Config controls one ingest run.
type Config struct {
	JetstreamHost   string        // e.g. jetstream.us-east.bsky.network
	JetstreamAPIKey string        // needed for archive replay
	StartBack       time.Duration // on first run, how far back to start (default 72h)
	FlushEvery      time.Duration // write at least this often
	FlushRows       int           // or once this many rows are pending
}

// Ingester runs the Jetstream → ClickHouse loop.
type Ingester struct {
	Cfg    Config
	Parser *Parser
	Writer *Writer
	Quotes *QuoteResolver
	Log    *slog.Logger
}

// Archive downloads are metered: Bluesky's hosted instances allow a 1GB burst, then
// refill at 2MB/s (from the headwind-quota-* response headers; not in the docs).
// Downloading a few whole segments at a time, one range request each, stays close to
// the refill rate instead of bursting past it.
const (
	downloadConcurrency = 2
	segmentStripes      = 1
	downloadAttempts    = 20
)

// While replaying history (more than this far behind live), any stream error
// triggers a restart from the last written position. The SDK treats a failed
// archive download as recoverable and skips past it, which would leave a hole.
const replayingLag = time.Hour

// errRestart asks Run to reconnect from the last written position.
var errRestart = errors.New("restart stream")

// Run consumes events until ctx is cancelled or the stream fails fatally.
func (in *Ingester) Run(ctx context.Context) error {
	after, skipBefore, err := in.startPoint(ctx)
	if err != nil {
		return err
	}
	backoff := time.Minute
	for {
		cursor, err := in.runOnce(ctx, after, skipBefore)
		if !errors.Is(err, errRestart) {
			return err
		}
		if cursor > after {
			after = cursor
			backoff = time.Minute // made progress since the last restart
		}
		in.Log.Warn("restarting stream from last written position", "after_seq", after, "wait", backoff.String())
		metricRestarts.Inc()
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 10*time.Minute)
	}
}

// runOnce streams from after until ctx ends, a fatal error, or a restart is needed.
// It returns the last sequence number whose rows are all written.
func (in *Ingester) runOnce(ctx context.Context, after uint64, skipBefore time.Time) (uint64, error) {
	in.Log.Info("starting stream", "after_seq", after, "skip_before", skipBefore)

	client, err := jetstream.Subscribe(in.Cfg.JetstreamHost,
		jetstream.WithAPIKey(in.Cfg.JetstreamAPIKey),
		jetstream.WithCollections(Collections),
		jetstream.WithKinds([]jetstream.Kind{jetstream.KindCommit, jetstream.KindAccount}),
		jetstream.WithAfterSeq(after),
		jetstream.WithBatchSize(1024),
		jetstream.WithDownloadConcurrency(downloadConcurrency),
		jetstream.WithSegmentStripes(segmentStripes),
		jetstream.WithMaxDownloadAttempts(downloadAttempts),
		jetstream.WithLogger(in.Log.With("component", "jetstream")),
	)
	if err != nil {
		return after, fmt.Errorf("subscribe: %w", err)
	}
	defer client.Close()

	var (
		rows       Rows
		lastCursor = after // everything up to here is written
		lastFlush  = time.Now()
		lastEvent  time.Time
		lastLog    = time.Now()
	)
	skipUS := skipBefore.UnixMicro()

	for batch, err := range client.Events(ctx) {
		if err != nil {
			if errors.Is(err, jetstream.ErrFatal) {
				return lastCursor, err
			}
			if ctx.Err() != nil {
				break
			}
			metricStreamErrors.Inc()
			in.Log.Warn("stream error", "err", err)
			if lastEvent.IsZero() || time.Since(lastEvent) > replayingLag {
				// Errors arrive in order, so everything handled so far precedes the
				// failed download. Write it, then reconnect from there.
				if err := in.flushDetached(ctx, &rows, lastCursor); err != nil {
					return lastCursor, err
				}
				return lastCursor, errRestart
			}
			continue
		}
		events := batch.Events()
		for i := range events {
			ev := &events[i]
			if ev.TimeUS < skipUS {
				continue
			}
			res := in.Parser.Handle(ev, &rows)
			metricEvents.WithLabelValues(res.Collection, res.Operation).Inc()
			if res.PostOutcome != "" {
				metricPosts.WithLabelValues(res.PostOutcome).Inc()
			}
			if res.Stale {
				metricStale.WithLabelValues(res.Collection).Inc()
			}
			lastEvent = time.UnixMicro(ev.TimeUS)
		}
		if c := batch.LastCursor(); c > 0 {
			lastCursor = c
		}
		if !lastEvent.IsZero() {
			metricLagSeconds.Set(time.Since(lastEvent).Seconds())
		}

		if rows.Len() >= in.Cfg.FlushRows || time.Since(lastFlush) >= in.Cfg.FlushEvery {
			if err := in.flushDetached(ctx, &rows, lastCursor); err != nil {
				return lastCursor, err
			}
			lastFlush = time.Now()
		}
		if time.Since(lastLog) >= 30*time.Second {
			st := client.Stats()
			metricArchiveGap.Set(float64(st.ResidualGap))
			attrs := []any{"cursor", lastCursor, "archive_remaining_seqs", st.ResidualGap, "text_cache", in.Parser.Texts.Len()}
			if !lastEvent.IsZero() {
				attrs = append(attrs, "event_time", lastEvent.UTC().Format(time.RFC3339), "lag", time.Since(lastEvent).Round(time.Second).String())
			}
			in.Log.Info("progress", attrs...)
			lastLog = time.Now()
		}
		if ctx.Err() != nil {
			break
		}
	}

	// Write whatever is pending on shutdown.
	if rows.Len() > 0 && lastCursor > 0 {
		if err := in.flushDetached(ctx, &rows, lastCursor); err != nil {
			return lastCursor, err
		}
	}
	if ctx.Err() != nil {
		return lastCursor, ctx.Err()
	}
	// The iterator ended without cancellation; reconnect rather than exit.
	return lastCursor, errRestart
}

// flushDetached runs a flush that a shutdown signal doesn't interrupt: once started,
// it gets up to two minutes to finish, so a signal never discards a half-done write.
func (in *Ingester) flushDetached(ctx context.Context, rows *Rows, cursor uint64) error {
	fctx, cancel := context.WithTimeout(context.WithoutCancel(ctx), 2*time.Minute)
	defer cancel()
	return in.flush(fctx, rows, cursor)
}

// startPoint returns the sequence number to replay after, and a time before which
// events are dropped (only on a first run, to trim the start to exactly StartBack).
func (in *Ingester) startPoint(ctx context.Context) (uint64, time.Time, error) {
	seq, ok, err := in.Writer.LoadCursor(ctx)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("load cursor: %w", err)
	}
	if ok {
		in.Log.Info("resuming from saved cursor", "seq", seq)
		return seq, time.Time{}, nil
	}
	start := time.Now().Add(-in.Cfg.StartBack)
	seq, err = SeqAtTime(ctx, in.Cfg.JetstreamHost, in.Cfg.JetstreamAPIKey, start)
	if err != nil {
		return 0, time.Time{}, fmt.Errorf("find start sequence: %w", err)
	}
	in.Log.Info("first run: starting from archive", "start_time", start.UTC().Format(time.RFC3339), "after_seq", seq)
	return seq, start, nil
}

// flush resolves quoted text, writes all pending rows, and then saves the cursor.
// It retries until it succeeds or ctx ends, so rows are never dropped and the cursor
// never moves past unwritten rows.
func (in *Ingester) flush(ctx context.Context, rows *Rows, cursor uint64) error {
	if rows.Len() == 0 && cursor == 0 {
		return nil
	}
	start := time.Now()
	in.Quotes.Resolve(ctx, rows)
	backoff := time.Second
	for {
		err := in.Writer.Write(ctx, rows)
		if err == nil && cursor > 0 {
			err = in.Writer.SaveCursor(ctx, cursor)
		}
		if err == nil {
			break
		}
		metricInsertErrors.Inc()
		in.Log.Error("flush failed; retrying", "err", err, "backoff", backoff)
		select {
		case <-ctx.Done():
			return fmt.Errorf("flush abandoned: %w", err)
		case <-time.After(backoff):
		}
		backoff = min(backoff*2, 30*time.Second)
	}
	metricFlushSeconds.Observe(time.Since(start).Seconds())
	metricCursor.Set(float64(cursor))
	rows.Reset()
	return nil
}
