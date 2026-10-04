package pipeline

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/prometheus/client_golang/prometheus"
	dto "github.com/prometheus/client_model/go"
)

// counterValue is a counter's current value, read the way the registry reads it.
func counterValue(c prometheus.Counter) float64 {
	var m dto.Metric
	if err := c.Write(&m); err != nil {
		panic(err)
	}
	return m.GetCounter().GetValue()
}

// statusTransport answers every request with one status and counts the requests.
type statusTransport struct {
	status int
	calls  atomic.Int64
}

func (s *statusTransport) RoundTrip(r *http.Request) (*http.Response, error) {
	s.calls.Add(1)
	return &http.Response{StatusCode: s.status, Status: http.StatusText(s.status), Header: http.Header{},
		Body: io.NopCloser(strings.NewReader("image")), Request: r}, nil
}

func testPipeline(rt http.RoundTripper) *Pipeline {
	return &Pipeline{Cfg: Config{MaxPictures: 2}, HTTP: &http.Client{Transport: rt},
		Log: slog.New(slog.NewTextHandler(io.Discard, nil))}
}

// A failed download counts as an error of the stage it was asked for, so the retry worker's
// failures (every retry of a deleted post fails) do not show up as live fetch errors.
func TestFetchPicturesCountsErrorsUnderTheirStage(t *testing.T) {
	refs := []pictureRef{{Kind: "image", CID: "bafyone"}, {Kind: "image", CID: "bafytwo"}}
	for _, stage := range []string{stageFetch, stageRetryFetch} {
		other := stageFetch
		if stage == stageFetch {
			other = stageRetryFetch
		}
		before := counterValue(metricErrors.WithLabelValues(stage))
		beforeOther := counterValue(metricErrors.WithLabelValues(other))

		rt := &statusTransport{status: http.StatusNotFound}
		pics, failed := testPipeline(rt).fetchPictures(context.Background(), stage, "did:plc:x", refs)
		if len(pics) != 0 || failed != 2 {
			t.Fatalf("%s: got %d pictures and %d failures from two 404s", stage, len(pics), failed)
		}
		if got := counterValue(metricErrors.WithLabelValues(stage)) - before; got != 2 {
			t.Errorf("%s errors grew by %v, want 2", stage, got)
		}
		if got := counterValue(metricErrors.WithLabelValues(other)) - beforeOther; got != 0 {
			t.Errorf("%s errors grew by %v when %s was asked for", other, got, stage)
		}
	}

	// Downloads that work count no errors at all.
	before := counterValue(metricErrors.WithLabelValues(stageRetryFetch))
	rt := &statusTransport{status: http.StatusOK}
	pics, failed := testPipeline(rt).fetchPictures(context.Background(), stageRetryFetch, "did:plc:x", refs)
	if len(pics) != 2 || failed != 0 || counterValue(metricErrors.WithLabelValues(stageRetryFetch)) != before {
		t.Errorf("working downloads: %d pictures, %d failed, errors %v -> %v", len(pics), failed, before, counterValue(metricErrors.WithLabelValues(stageRetryFetch)))
	}
}
