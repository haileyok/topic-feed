package pipeline

import (
	"github.com/prometheus/client_golang/prometheus"
	"github.com/prometheus/client_golang/prometheus/promauto"
)

var (
	metricPosts = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pipeline_posts_total",
		Help: "Posts processed, by feed policy (ok, adult_only, drop).",
	}, []string{"policy"})

	metricPictures = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pipeline_pictures_total",
		Help: "Pictures (images and video poster frames) the model should look at, by download result (fetched, failed).",
	}, []string{"result"})

	metricFetchSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "pipeline_picture_fetch_seconds", Help: "Time to download one picture.",
		Buckets: prometheus.ExponentialBuckets(0.02, 2, 10),
	})

	metricRetries = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pipeline_image_retries_total",
		Help: "Picture retry results per post: fixed, failed (will retry), gave_up.",
	}, []string{"result"})

	metricRetryPending = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "pipeline_image_retry_pending", Help: "Posts waiting in image_retry_queue.",
	})

	metricBatchSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "pipeline_batch_seconds", Help: "Time to process and write one batch.",
		Buckets: prometheus.ExponentialBuckets(0.05, 2, 12),
	})

	metricLag = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "pipeline_lag_seconds", Help: "Seconds between now and the newest processed post's ingest time.",
	})

	metricClassifySeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "pipeline_classify_seconds", Help: "Time for one classifier service call (up to 512 posts), including retries.",
		Buckets: prometheus.ExponentialBuckets(0.01, 2, 12),
	})

	metricClassified = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pipeline_classified_total", Help: "Posts classified, by top broad topic.",
	}, []string{"broad"})

	metricErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pipeline_errors_total", Help: "Errors, by stage.",
	}, []string{"stage"})
)
