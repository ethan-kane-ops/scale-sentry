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

func TestBuildLimiters(t *testing.T) {
	tests := []struct {
		name            string
		targetRPS       int
		stripes         int
		wantCount       int
		wantTotalRate   float64
		wantPerBurstMin int
	}{
		{
			name:            "default stripe count",
			targetRPS:       1000,
			stripes:         8,
			wantCount:       8,
			wantTotalRate:   1000,
			wantPerBurstMin: 1,
		},
		{
			name:            "collapse stripes when targetRPS smaller",
			targetRPS:       3,
			stripes:         8,
			wantCount:       3,
			wantTotalRate:   3,
			wantPerBurstMin: 1,
		},
		{
			name:            "single stripe when stripes <= 0",
			targetRPS:       100,
			stripes:         0,
			wantCount:       1,
			wantTotalRate:   100,
			wantPerBurstMin: 1,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			ls := buildLimiters(tt.targetRPS, tt.stripes)
			if len(ls) != tt.wantCount {
				t.Fatalf("len = %d, want %d", len(ls), tt.wantCount)
			}
			var total float64
			for _, l := range ls {
				total += float64(l.Limit())
				if l.Burst() < tt.wantPerBurstMin {
					t.Errorf("per-stripe burst = %d, want >= %d", l.Burst(), tt.wantPerBurstMin)
				}
			}
			// Float division: allow tiny rounding slack.
			if diff := total - tt.wantTotalRate; diff > 0.001 || diff < -0.001 {
				t.Errorf("summed rate = %v, want %v", total, tt.wantTotalRate)
			}
		})
	}
}

func TestCollectorLatencyHistogram(t *testing.T) {
	c := newCollector()
	c.start = time.Now()
	// 100 samples uniformly spread from 1ms to 100ms.
	for i := 1; i <= 100; i++ {
		c.recordStatus(200, time.Duration(i)*time.Millisecond, c.start)
	}
	c.end = c.start.Add(time.Second)
	r := c.finalize(Config{ConnectionMode: KeepAlive})

	// HDR is approximate; allow a few-ms slack at each percentile.
	within := func(name string, got, want, slack time.Duration) {
		t.Helper()
		diff := got - want
		if diff < 0 {
			diff = -diff
		}
		if diff > slack {
			t.Errorf("%s = %v, want ~%v (±%v)", name, got, want, slack)
		}
	}
	within("p50", r.LatencyP50, 50*time.Millisecond, 2*time.Millisecond)
	within("p95", r.LatencyP95, 95*time.Millisecond, 2*time.Millisecond)
	within("p99", r.LatencyP99, 99*time.Millisecond, 2*time.Millisecond)
	within("max", r.LatencyMax, 100*time.Millisecond, 2*time.Millisecond)
}

func TestCollectorLatencyEmpty(t *testing.T) {
	c := newCollector()
	c.start = time.Now()
	c.end = c.start
	r := c.finalize(Config{ConnectionMode: KeepAlive})
	if r.LatencyP50 != 0 || r.LatencyP95 != 0 || r.LatencyP99 != 0 || r.LatencyMax != 0 {
		t.Errorf("empty run percentiles = (%v, %v, %v, %v), want all zero",
			r.LatencyP50, r.LatencyP95, r.LatencyP99, r.LatencyMax)
	}
}

func TestCollectorLatencyClamp(t *testing.T) {
	// Values above the histogram max (60s) clamp to max rather than
	// vanishing — a 90s timeout must still show up at the tail.
	c := newCollector()
	c.start = time.Now()
	c.recordError(ErrTimeout, 90*time.Second, c.start)
	c.end = c.start.Add(time.Second)
	r := c.finalize(Config{ConnectionMode: KeepAlive})
	if r.LatencyMax < 59*time.Second || r.LatencyMax > 61*time.Second {
		t.Errorf("max = %v, want clamp to ~60s", r.LatencyMax)
	}
}
