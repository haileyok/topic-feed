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

	metricImages = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pipeline_image_texts_total",
		Help: "Images and videos without alt text, by where their text came from (ocr, luna, none, budget, error).",
	}, []string{"source"})

	metricOCRSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "pipeline_ocr_seconds", Help: "Time to download and OCR one thumbnail.",
		Buckets: prometheus.ExponentialBuckets(0.05, 2, 10),
	})

	metricLLMSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "pipeline_llm_seconds", Help: "Time for one LLM image description.",
		Buckets: prometheus.ExponentialBuckets(0.25, 2, 10),
	})

	metricLLMCost = promauto.NewCounter(prometheus.CounterOpts{
		Name: "pipeline_llm_cost_usd_total", Help: "List-price cost of LLM image descriptions.",
	})

	metricLLMSpentToday = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "pipeline_llm_spent_today_usd", Help: "List-price LLM spending so far today (UTC), counted against the daily budget.",
	})

	metricBatchSeconds = promauto.NewHistogram(prometheus.HistogramOpts{
		Name: "pipeline_batch_seconds", Help: "Time to process and write one batch.",
		Buckets: prometheus.ExponentialBuckets(0.05, 2, 12),
	})

	metricLag = promauto.NewGauge(prometheus.GaugeOpts{
		Name: "pipeline_lag_seconds", Help: "Seconds between now and the newest processed post's ingest time.",
	})

	metricErrors = promauto.NewCounterVec(prometheus.CounterOpts{
		Name: "pipeline_errors_total", Help: "Errors, by stage.",
	}, []string{"stage"})
)
