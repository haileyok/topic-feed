package modlabels

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	metricLabels = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "modlabels_labels_total",
		Help: "Labels received, by value and whether they remove an earlier label.",
	}, []string{"val", "neg"})

	metricFrames = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "modlabels_frames_total",
		Help: "Stream messages received, by kind (#labels, #info, error, other).",
	}, []string{"kind"})

	metricRowsWritten = promauto.NewCounter(prometheus.CounterOpts{
		Name: "modlabels_rows_written_total",
		Help: "Label rows written to ClickHouse.",
	})

	metricFlushErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "modlabels_flush_errors_total",
		Help: "Failed ClickHouse flushes (each is retried).",
	})

	metricReconnects = promauto.NewCounter(prometheus.CounterOpts{
		Name: "modlabels_reconnects_total",
		Help: "Stream reconnects after an error or an idle timeout.",
	})

	metricLag = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "modlabels_lag_seconds",
		Help: "Seconds between the newest label's creation time and when it was received.",
	})

	metricSeq = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "modlabels_last_seq",
		Help: "Sequence number of the last label frame written.",
	})
)
