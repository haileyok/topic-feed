package ingest

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	metricEvents = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ingest_events_total",
		Help: "Jetstream events handled, by collection (or event kind) and operation.",
	}, []string{"collection", "operation"})

	metricPosts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ingest_post_creates_total",
		Help: "Post creates by outcome: kept, reply, not_tagged_en, detector_disagrees.",
	}, []string{"outcome"})

	metricStale = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ingest_stale_records_total",
		Help: "Created records dropped because they are more than 24h older than their event (account resyncs, backdated imports), by collection.",
	}, []string{"collection"})

	metricRowsWritten = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ingest_rows_written_total",
		Help: "Rows written to ClickHouse, by table.",
	}, []string{"table"})

	metricFlushSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "ingest_flush_seconds",
		Help:    "Time to resolve quotes and write one flush to ClickHouse.",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 12),
	})

	metricInsertErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ingest_insert_errors_total",
		Help: "Failed ClickHouse flushes (each is retried).",
	})

	metricQuotes = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "ingest_quote_lookups_total",
		Help: "Quoted-post text lookups, by where the text was found: memory, clickhouse, appview, missing.",
	}, []string{"source"})

	metricLagSeconds = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ingest_lag_seconds",
		Help: "Now minus the time of the last event handled.",
	})

	metricCursor = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ingest_cursor",
		Help: "Last Jetstream sequence number saved to ingest_cursor.",
	})

	metricStreamErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "ingest_stream_errors_total",
		Help: "Recoverable errors reported by the Jetstream client.",
	})

	metricArchiveGap = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "ingest_archive_remaining_seqs",
		Help: "Sealed-archive sequence numbers still to download before switching to live (0 once live).",
	})
)
