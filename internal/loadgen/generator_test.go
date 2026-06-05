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
		t.Errorf("Sent = %d, want roughly 50 (range 25 to 65)", result.Sent)
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

// TestGeneratorRun_WarmupExcludedFromHistogram asserts that requests sent
// during a phase with RecordStats=false are dispatched against the target
// (so caches / TLS / JIT warm up) but absent from the latency histogram +
// counters used for the SLA verdict.
func TestGeneratorRun_WarmupExcludedFromHistogram(t *testing.T) {
	var hits int64
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		atomic.AddInt64(&hits, 1)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	g, err := New(Config{
		URL:            srv.URL + "/",
		ConnectionMode: KeepAlive,
		Phases: []Phase{
			{Name: WarmupPhaseName, Pattern: PatternConstant, Duration: 400 * time.Millisecond, StartRPS: 50, RecordStats: false},
			{Name: MeasurePhaseName, Pattern: PatternConstant, Duration: 400 * time.Millisecond, StartRPS: 50, RecordStats: true},
		},
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result := g.Run(context.Background())

	total := atomic.LoadInt64(&hits)
	if total == 0 {
		t.Fatal("server saw zero requests; warmup phase didn't fire")
	}
	if result.Sent == 0 {
		t.Fatal("Sent = 0; measure phase recorded nothing")
	}
	if int64(result.Sent) >= total {
		t.Errorf("Sent = %d, want < server-observed %d (warmup should NOT count)", result.Sent, total)
	}
	if result.WarmupDuration <= 0 {
		t.Errorf("WarmupDuration = %v, want > 0", result.WarmupDuration)
	}
	if result.MeasurementDuration <= 0 {
		t.Errorf("MeasurementDuration = %v, want > 0", result.MeasurementDuration)
	}
	if len(result.Phases) != 2 {
		t.Fatalf("Phases length = %d, want 2", len(result.Phases))
	}
	if result.Phases[0].Name != WarmupPhaseName || result.Phases[0].RecordStats {
		t.Errorf("Phases[0] = %+v, want Warmup with RecordStats=false", result.Phases[0])
	}
	if !result.Phases[1].RecordStats {
		t.Errorf("Phases[1] = %+v, want Measure with RecordStats=true", result.Phases[1])
	}
}

// TestGeneratorRun_ScheduledArrivalLatency asserts the coordinated-omission
// fix is real: when the target stalls (handler artificially slow), the
// recorded latencies include the queue wait of subsequent scheduled
// arrivals, NOT just the per-request response time. Pre-fix, every
// recorded latency would have hovered around the handler's stall window
// regardless of overload; the fix makes later latencies climb.
func TestGeneratorRun_ScheduledArrivalLatency(t *testing.T) {
	// Handler that always sleeps 200ms: a single worker can only complete
	// 5 requests/second. Asking for 50 RPS guarantees backlog.
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		time.Sleep(200 * time.Millisecond)
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	g, err := New(Config{
		URL:            srv.URL + "/",
		TargetRPS:      50,
		Duration:       1500 * time.Millisecond,
		Concurrency:    4, // intentionally too small for 50 RPS at 200ms/req
		ConnectionMode: KeepAlive,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result := g.Run(context.Background())

	if result.Sent == 0 {
		t.Fatal("Sent = 0")
	}
	// Under coordinated-omission-aware measurement, the tail latency
	// must reflect the queue wait, not just the 200ms handler stall.
	// Pre-fix p99 would have sat near the 200ms handler time; post-fix
	// it climbs well past it as later scheduled arrivals accumulate
	// queue depth.
	if result.LatencyP99 < 400*time.Millisecond {
		t.Errorf("LatencyP99 = %v, want >> 200ms handler stall (coordinated-omission fix should make tail climb under backlog)", result.LatencyP99)
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
	// vanishing, a 90s timeout must still show up at the tail.
	c := newCollector()
	c.start = time.Now()
	c.recordError(ErrTimeout, 90*time.Second, c.start)
	c.end = c.start.Add(time.Second)
	r := c.finalize(Config{ConnectionMode: KeepAlive})
	if r.LatencyMax < 59*time.Second || r.LatencyMax > 61*time.Second {
		t.Errorf("max = %v, want clamp to ~60s", r.LatencyMax)
	}
}
