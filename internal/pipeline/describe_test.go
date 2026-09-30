package pipeline

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

const okBody = `{"choices":[{"message":{"content":"A cat on a sofa."}}],"usage":{"prompt_tokens":100,"completion_tokens":10}}`

func TestDescribeRetriesServerErrorOnce(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if hits.Add(1) == 1 {
			http.Error(w, "upstream reset", http.StatusBadGateway)
			return
		}
		w.Write([]byte(okBody))
	}))
	defer srv.Close()
	d := &Describer{Endpoint: srv.URL, Client: srv.Client(), Budget: NewBudget(100, 0), Workers: 2, Timeout: time.Second}
	desc, _, err := d.Describe(context.Background(), "data:image/jpeg;base64,AA==")
	if err != nil || desc != "A cat on a sofa." {
		t.Fatalf("got %q, %v", desc, err)
	}
	if hits.Load() != 2 {
		t.Errorf("%d requests, want 2 (one retry)", hits.Load())
	}
}

func TestDescribeDoesNotRetryClientError(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		http.Error(w, "bad request", http.StatusBadRequest)
	}))
	defer srv.Close()
	d := &Describer{Endpoint: srv.URL, Client: srv.Client(), Budget: NewBudget(100, 0), Workers: 2, Timeout: time.Second}
	if _, _, err := d.Describe(context.Background(), "x"); err == nil {
		t.Fatal("want an error")
	}
	if hits.Load() != 1 {
		t.Errorf("%d requests, want 1", hits.Load())
	}
}

func TestDescribePausesAfterRepeatedTimeouts(t *testing.T) {
	var hits atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
		select { // slower than the timeout
		case <-time.After(time.Second):
		case <-r.Context().Done():
		}
	}))
	defer srv.Close()
	d := &Describer{Endpoint: srv.URL, Client: srv.Client(), Budget: NewBudget(100, 0), Workers: 8,
		Timeout: 20 * time.Millisecond, Pause: 200 * time.Millisecond}
	for i := 0; i < breakerMinCalls; i++ {
		if _, _, err := d.Describe(context.Background(), "x"); err == nil || errors.Is(err, ErrUnavailable) {
			t.Fatalf("call %d: want a timeout, got %v", i, err)
		}
	}
	before := hits.Load()
	start := time.Now()
	if _, _, err := d.Describe(context.Background(), "x"); !errors.Is(err, ErrUnavailable) {
		t.Fatalf("want ErrUnavailable while paused, got %v", err)
	}
	if time.Since(start) > 10*time.Millisecond || hits.Load() != before {
		t.Error("a paused call should return at once without a request")
	}
	time.Sleep(250 * time.Millisecond)
	if _, _, err := d.Describe(context.Background(), "x"); errors.Is(err, ErrUnavailable) {
		t.Error("descriptions should resume after the pause")
	}
}
