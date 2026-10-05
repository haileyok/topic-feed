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

	metricFilteredPosts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "feedgen_filtered_posts_total",
		Help: "Posts read from filtered feeds' sources (read), and of those the ones the filters left out (dropped), by feed.",
	}, []string{"feed", "kind"})

	metricFilteredPages = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "feedgen_filtered_pages_total",
		Help: "Pages of filtered feeds served, by feed and state (ok, signin: only the sign-in post).",
	}, []string{"feed", "state"})

	metricFilteredSource = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "feedgen_filtered_source_requests_total",
		Help: "Requests to filtered feeds' sources, by filtered feed and HTTP status (or error).",
	}, []string{"feed", "status"})

	metricFilteredSourceSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "feedgen_filtered_source_seconds",
		Help:    "How long filtered feeds' sources take to answer, by filtered feed.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2, 4},
	}, []string{"feed"})

	metricForwardedInteractions = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "feedgen_forwarded_interactions_total",
		Help: "Interactions sent on to filtered feeds' sources, by filtered feed and outcome (ok, signin, error, dropped).",
	}, []string{"feed", "result"})

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

	metricPersonalPages = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "feedgen_personal_pages_total",
		Help: "Pages of personal feeds, by feed and what the viewer got (personal, generic, welcome).",
	}, []string{"feed", "state"})

	metricPersonalViewers = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "feedgen_personal_viewers",
		Help: "Viewers whose personal feeds are held in memory.",
	})

	metricPersonalBuildSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name:    "feedgen_personal_viewer_build_seconds",
		Help:    "Time to read a new viewer's likes and what they have been shown.",
		Buckets: []float64{.05, .1, .25, .5, 1, 1.5, 2.5, 5, 10, 30},
	})

	metricPersonalBuildErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "feedgen_personal_viewer_build_errors_total",
		Help: "Viewers whose likes or seen posts couldn't be read (they see the welcome post, then a retry).",
	})

	metricPersonalTuningErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "feedgen_personal_tuning_errors_total",
		Help: "Failed reads of a viewer's saved tuning (their feed is untuned until a retry succeeds).",
	})

	metricPersonalPoolSeconds = promauto.NewHistogramVec(prometheus.HistogramOpts{
		Name:    "feedgen_personal_pool_seconds",
		Help:    "Time to read the posts personal feeds draw on.",
		Buckets: []float64{.05, .1, .25, .5, 1, 2.5, 5, 10},
	}, []string{"feed"})

	metricPersonalPoolErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "feedgen_personal_pool_errors_total",
		Help: "Failed reads of a personal feed's posts (the previous ones keep being used).",
	}, []string{"feed"})

	metricPersonalPoolPosts = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "feedgen_personal_pool_posts",
		Help: "Posts a personal feed draws on after the latest read.",
	}, []string{"feed"})

	metricServedWritten = promauto.NewCounter(prometheus.CounterOpts{
		Name: "feedgen_served_written_total",
		Help: "Served-post rows written to ClickHouse.",
	})

	metricServedDropped = promauto.NewCounter(prometheus.CounterOpts{
		Name: "feedgen_served_dropped_total",
		Help: "Served-post rows dropped: the write queue was full, or ClickHouse was down too long.",
	})

	metricServedWriteErrors = promauto.NewCounter(prometheus.CounterOpts{
		Name: "feedgen_served_write_errors_total",
		Help: "Failed served-post writes (retried on the next flush).",
	})

	metricLastRefresh = promauto.NewGaugeVec(prometheus.GaugeOpts{
		Name: "feedgen_last_refresh_timestamp_seconds",
		Help: "Unix time of each feed's latest successful rebuild.",
	}, []string{"feed"})
)
