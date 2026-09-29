package feedgen

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	metricRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "feedgen_skeleton_requests_total",
		Help: "Feed skeleton requests, by feed (rkey, or 'unknown') and HTTP status.",
	}, []string{"feed", "status"})

	metricAuth = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "feedgen_viewer_auth_total",
		Help: "Viewer credentials on skeleton requests: ok, invalid (served anyway), or none.",
	}, []string{"result"})

	metricRequestSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "feedgen_request_seconds",
		Help:    "HTTP request duration, by route.",
		Buckets: []float64{.001, .005, .01, .05, .1, .25, .5, 1, 2.5},
	}, []string{"route"})

	metricFeedPosts = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "feedgen_feed_posts",
		Help: "Posts in each feed after the latest refresh.",
	}, []string{"feed"})

	metricRemoved = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "feedgen_feed_removed_posts",
		Help: "Candidates left out of each feed in the latest refresh, by reason (deleted, inactive, labeled).",
	}, []string{"feed", "reason"})

	metricRefreshSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "feedgen_refresh_seconds",
		Help:    "Time to rebuild a feed from ClickHouse.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"feed"})

	metricRefreshErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "feedgen_refresh_errors_total",
		Help: "Failed feed rebuilds (the previous posts keep being served).",
	}, []string{"feed"})

	metricInteractions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "feedgen_interactions_total",
		Help: "Interactions received from Bluesky, by feed and event (interactionSeen, requestLess, ...).",
	}, []string{"feed", "event"})

	metricInteractionRequests = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "feedgen_interaction_requests_total",
		Help: "sendInteractions calls, by HTTP status.",
	}, []string{"status"})

	metricInteractionsWritten = promauto.NewCounter(prometheus.CounterOpts{
		Name: "feedgen_interactions_written_total",
		Help: "Interaction rows written to ClickHouse.",
	})

	metricInteractionsDropped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "feedgen_interactions_dropped_total",
		Help: "Interactions dropped: the write queue was full, or ClickHouse was down too long.",
	})

	metricInteractionWriteErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "feedgen_interaction_write_errors_total",
		Help: "Failed interaction writes (retried on the next flush).",
	})

	metricPreviews = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "feedgen_previews_total",
		Help: "Feed builder previews, by outcome (OK, Too Many Requests, Bad Request, ...).",
	}, []string{"result"})

	metricPreviewSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "feedgen_preview_build_seconds",
		Help:    "Time to build and rank a feed builder preview (cache misses).",
		Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10},
	})

	metricLastRefresh = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "feedgen_last_refresh_timestamp_seconds",
		Help: "Unix time of each feed's latest successful rebuild.",
	}, []string{"feed"})
)
