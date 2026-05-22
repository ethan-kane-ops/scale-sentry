package loadgen

import (
	"context"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// TestGeneratorRun_HitsTarget runs a short load against an httptest server
// and asserts the generator records ~target RPS within tolerance, classifies
// 2xx as success, and populates labels.
func TestGeneratorRun_HitsTarget(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	cfg := Config{
		URL:            srv.URL + "/",
		TargetRPS:      50,
		Duration:       1 * time.Second,
		ConnectionMode: KeepAlive,
		TargetMode:     TargetServiceDefault,
		NetworkPath:    PathClusterIP,
	}

	g, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	result := g.Run(context.Background())

	// Tolerance: rate limiter holds steady but may undershoot due to
	// goroutine startup, server response time, etc. Accept 50% to 130%.
	if result.Sent < 25 || result.Sent > 65 {
		t.Errorf("Sent = %d, want roughly 50 (range 25–65)", result.Sent)
	}
	if result.Failed != 0 {
		t.Errorf("Failed = %d, want 0; errors=%v", result.Failed, result.Errors)
	}
	if result.StatusCounts[200] != result.Succeeded {
		t.Errorf("StatusCounts[200] = %d, Succeeded = %d", result.StatusCounts[200], result.Succeeded)
	}
	if result.Labels["connectionMode"] != "KeepAlive" {
		t.Errorf("Labels[connectionMode] = %q, want KeepAlive", result.Labels["connectionMode"])
	}
	if result.Labels["targetMode"] != "ServiceDefault" {
		t.Errorf("Labels[targetMode] = %q, want ServiceDefault", result.Labels["targetMode"])
	}
	if result.Labels["networkPath"] != "ClusterIP" {
		t.Errorf("Labels[networkPath] = %q, want ClusterIP", result.Labels["networkPath"])
	}
	if result.LatencyP50 <= 0 {
		t.Errorf("LatencyP50 = %v, want > 0", result.LatencyP50)
	}
}

func TestGeneratorRun_RecordsServer5xxAsFailure(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusInternalServerError)
	}))
	defer srv.Close()

	g, err := New(Config{
		URL:            srv.URL + "/",
		TargetRPS:      30,
		Duration:       500 * time.Millisecond,
		ConnectionMode: KeepAlive,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result := g.Run(context.Background())
	if result.Sent == 0 {
		t.Fatal("Sent = 0, expected requests")
	}
	if result.Failed != result.Sent {
		t.Errorf("Failed = %d, want %d (all 5xx)", result.Failed, result.Sent)
	}
	if result.Errors[ErrServer] != result.Sent {
		t.Errorf("Errors[Server5xx] = %d, want %d", result.Errors[ErrServer], result.Sent)
	}
	if rate := result.FailureRate(); rate != 1.0 {
		t.Errorf("FailureRate = %v, want 1.0", rate)
	}
	if int64(len(result.ErrorSamples)) != result.Sent {
		t.Errorf("ErrorSamples count = %d, want %d (one per failed request)",
			len(result.ErrorSamples), result.Sent)
	}
	for i, s := range result.ErrorSamples {
		if s.Category != ErrServer {
			t.Errorf("ErrorSamples[%d].Category = %q, want Server5xx", i, s.Category)
		}
		if s.Status != http.StatusInternalServerError {
			t.Errorf("ErrorSamples[%d].Status = %d, want 500", i, s.Status)
		}
		if s.At.Before(result.Started) {
			t.Errorf("ErrorSamples[%d].At = %v, before run start %v", i, s.At, result.Started)
		}
	}
}

func TestGeneratorRun_NoErrorSamplesOnClean(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	g, err := New(Config{
		URL:            srv.URL + "/",
		TargetRPS:      30,
		Duration:       300 * time.Millisecond,
		ConnectionMode: KeepAlive,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result := g.Run(context.Background())
	if len(result.ErrorSamples) != 0 {
		t.Errorf("ErrorSamples = %v, want empty on a clean run", result.ErrorSamples)
	}
}

func TestGeneratorRun_ContextCancel(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	g, err := New(Config{
		URL:            srv.URL + "/",
		TargetRPS:      50,
		Duration:       10 * time.Second, // long; will be cut short by ctx
		ConnectionMode: KeepAlive,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 200*time.Millisecond)
	defer cancel()
	start := time.Now()
	result := g.Run(ctx)
	elapsed := time.Since(start)

	if elapsed > 2*time.Second {
		t.Errorf("Run took %v, expected ~200ms (context cancelled early)", elapsed)
	}
	if result.Sent < 1 {
		t.Errorf("Sent = %d, expected at least one request before cancellation", result.Sent)
	}
}

func TestGeneratorRun_ShortLivedConnectionMode(t *testing.T) {
	var closeRequested int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Connection") == "close" {
			atomic.AddInt64(&closeRequested, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	g, err := New(Config{
		URL:            srv.URL + "/",
		TargetRPS:      20,
		Duration:       500 * time.Millisecond,
		ConnectionMode: ShortLived,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result := g.Run(context.Background())

	if result.Sent == 0 {
		t.Fatal("Sent = 0")
	}
	got := atomic.LoadInt64(&closeRequested)
	if got != result.Sent {
		t.Errorf("Connection: close header on %d of %d requests, want all", got, result.Sent)
	}
	if result.Labels["connectionMode"] != "ShortLived" {
		t.Errorf("Labels[connectionMode] = %q, want ShortLived", result.Labels["connectionMode"])
	}
}

func TestPercentilesEmpty(t *testing.T) {
	p50, p95, p99, max := percentiles(nil)
	if p50 != 0 || p95 != 0 || p99 != 0 || max != 0 {
		t.Errorf("percentiles(nil) = (%v, %v, %v, %v), want all zero", p50, p95, p99, max)
	}
}

func TestPercentilesOrdered(t *testing.T) {
	samples := make([]time.Duration, 100)
	for i := range samples {
		samples[i] = time.Duration(i+1) * time.Millisecond
	}
	p50, p95, p99, max := percentiles(samples)
	if p50 != 50*time.Millisecond {
		t.Errorf("p50 = %v, want 50ms", p50)
	}
	if p95 != 95*time.Millisecond {
		t.Errorf("p95 = %v, want 95ms", p95)
	}
	if p99 != 99*time.Millisecond {
		t.Errorf("p99 = %v, want 99ms", p99)
	}
	if max != 100*time.Millisecond {
		t.Errorf("max = %v, want 100ms", max)
	}
}
