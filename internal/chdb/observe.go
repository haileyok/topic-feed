package chdb

import (
	"context"
	"maps"
	"sync/atomic"
	"time"

	"github.com/ClickHouse/clickhouse-go/v2"
	"github.com/ClickHouse/clickhouse-go/v2/lib/driver"
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

// Every query a service makes is named twice over, so its cost can be traced back to the code that
// made it:
//   - in ClickHouse's own query log, log_comment is "<service>/<operation>" (system.query_log), and
//   - in the service's Prometheus metrics, clickhouse_client_* count each operation's queries, time,
//     errors, and what the server spent on them: rows and bytes read, CPU time and peak memory.
//
// The operation is what Op put on the context, "other" when nothing did.

var (
	metricQuerySeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "clickhouse_client_query_seconds",
		Help:    "Time of ClickHouse queries this service made, by operation (for streamed results, until the first block).",
		Buckets: []float64{.001, .005, .01, .025, .05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"op"})
	metricReadRows = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "clickhouse_client_read_rows_total",
		Help: "Rows ClickHouse read for this service's queries, by operation.",
	}, []string{"op"})
	metricReadBytes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "clickhouse_client_read_bytes_total",
		Help: "Bytes (uncompressed) ClickHouse read for this service's queries, by operation.",
	}, []string{"op"})
	metricCPUSeconds = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "clickhouse_client_cpu_seconds_total",
		Help: "CPU time ClickHouse spent on this service's queries (all its threads), by operation.",
	}, []string{"op"})
	metricPeakMemory = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "clickhouse_client_query_peak_memory_bytes",
		Help:    "Most memory ClickHouse used at once for each of this service's queries, by operation.",
		Buckets: prometheus.ExponentialBuckets(1<<20, 4, 8), // 1 MiB to 16 GiB
	}, []string{"op"})
	metricQueryErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "clickhouse_client_query_errors_total",
		Help: "ClickHouse queries of this service that failed, by operation.",
	}, []string{"op"})
)

type opKey struct{}

type opInfo struct {
	name     string
	settings clickhouse.Settings
}

// Op names the queries made with the returned context: "feed_build", "score_lookup", and so on.
// Inside another Op the name is added to the outer one's ("feed_build.deleted"), so shared code is
// told apart by who called it. Settings for those queries go here too, not in a clickhouse.Context
// of their own: the driver keeps only the latest settings put on a context, and Op merges them with
// the name and with the settings of the outer Op.
func Op(ctx context.Context, name string, settings ...clickhouse.Settings) context.Context {
	merged := clickhouse.Settings{}
	if prev, ok := ctx.Value(opKey{}).(opInfo); ok {
		maps.Copy(merged, prev.settings)
		name = prev.name + "." + name
	}
	for _, s := range settings {
		maps.Copy(merged, s)
	}
	merged["log_comment"] = app + "/" + name
	ctx = context.WithValue(ctx, opKey{}, opInfo{name: name, settings: merged})
	return clickhouse.Context(ctx, clickhouse.WithSettings(merged))
}

func opName(ctx context.Context) string {
	if o, ok := ctx.Value(opKey{}).(opInfo); ok {
		return o.name
	}
	return "other"
}

// observed is a connection that measures every query it makes (see the top of this file).
type observed struct {
	driver.Conn
}

// track adds a callback to ctx that counts what the server read and the CPU it spent, from the
// profile events it sends with the query's results, and returns a function to call when the query is
// done. The server sends the query's totals as thread 0, as increments since its last packet (and
// memory as gauges); they add up to what system.query_log has for the query. The driver keeps one
// profile-events callback per context, so code that sets its own loses these metrics (none does);
// WithProgress is left to callers.
func track(ctx context.Context) (context.Context, func(error)) {
	op := opName(ctx)
	rows, bytes, cpu := metricReadRows.WithLabelValues(op), metricReadBytes.WithLabelValues(op), metricCPUSeconds.WithLabelValues(op)
	var peak atomic.Int64
	ctx = clickhouse.Context(ctx, clickhouse.WithProfileEvents(func(events []clickhouse.ProfileEvent) {
		for _, e := range events {
			if e.ThreadID != 0 {
				continue
			}
			switch e.Name {
			case "SelectedRows":
				rows.Add(float64(e.Value))
			case "SelectedBytes":
				bytes.Add(float64(e.Value))
			case "OSCPUVirtualTimeMicroseconds":
				cpu.Add(float64(e.Value) / 1e6)
			case "MemoryTrackerPeakUsage":
				peak.Store(e.Value)
			}
		}
	}))
	start := time.Now()
	return ctx, func(err error) {
		metricQuerySeconds.WithLabelValues(op).Observe(time.Since(start).Seconds())
		if p := peak.Load(); p > 0 {
			metricPeakMemory.WithLabelValues(op).Observe(float64(p))
		}
		if err != nil {
			metricQueryErrors.WithLabelValues(op).Inc()
		}
	}
}

func (c observed) Select(ctx context.Context, dest any, query string, args ...any) error {
	ctx, done := track(ctx)
	err := c.Conn.Select(ctx, dest, query, args...)
	done(err)
	return err
}

func (c observed) Query(ctx context.Context, query string, args ...any) (driver.Rows, error) {
	ctx, done := track(ctx)
	rows, err := c.Conn.Query(ctx, query, args...)
	done(err)
	return rows, err
}

func (c observed) QueryRow(ctx context.Context, query string, args ...any) driver.Row {
	ctx, done := track(ctx)
	row := c.Conn.QueryRow(ctx, query, args...)
	done(row.Err())
	return row
}

func (c observed) Exec(ctx context.Context, query string, args ...any) error {
	ctx, done := track(ctx)
	err := c.Conn.Exec(ctx, query, args...)
	done(err)
	return err
}
