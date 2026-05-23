package loadgen

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"time"

	hdrhistogram "github.com/HdrHistogram/hdrhistogram-go"
	"github.com/valyala/fasthttp"
	"golang.org/x/time/rate"
)

// Latency histogram bounds. 1µs..60s with 3 significant figures matches
// the operational range scale-sentry runs in (sub-ms hot paths through to
// timeouts) and keeps the per-histogram footprint to a few KB — orders of
// magnitude smaller than an unbounded []time.Duration that grew to 50M+
// entries on long high-RPS runs and dominated the Job pod's memory.
const (
	latencyHistogramMin    = int64(time.Microsecond / time.Nanosecond)
	latencyHistogramMax    = int64(60 * time.Second / time.Nanosecond)
	latencyHistogramSigFig = 3
)

// limiterStripes is the number of independent rate.Limiter instances the
// generator allocates. Workers round-robin across the stripes, splitting
// the global TargetRPS budget evenly. A single shared limiter has its
// internal mutex contended on every Wait() call — at high RPS with
// hundreds of workers this contention dominates the run and starves the
// HTTP path of CPU, throttling actual throughput well below TargetRPS.
// Eight stripes is the smallest power-of-two that drops contention by
// roughly an order of magnitude while keeping per-stripe burst behaviour
// close to the original single-limiter shape.
const limiterStripes = 8

// Generator drives a single concurrent prober run. One Generator runs
// exactly one URL with one ConnectionMode — re-use across runs is not
// supported.
type Generator struct {
	cfg      Config
	client   *fasthttp.Client
	limiters []*rate.Limiter
}

// New constructs a Generator. Validates cfg and returns any error encountered.
func New(cfg Config) (*Generator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()
	client, err := newClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("build client: %w", err)
	}
	return &Generator{
		cfg:      cfg,
		client:   client,
		limiters: buildLimiters(cfg.TargetRPS, limiterStripes),
	}, nil
}

// buildLimiters allocates `stripes` independent rate.Limiter instances
// whose summed rate equals targetRPS and whose summed burst equals the
// original single-limiter burst (targetRPS/10, floor 1). When targetRPS
// is smaller than the requested stripe count the limiter array collapses
// to targetRPS limiters of 1 RPS each, since fewer-than-1-RPS stripes
// have no token-refill cadence at all.
func buildLimiters(targetRPS, stripes int) []*rate.Limiter {
	if stripes < 1 {
		stripes = 1
	}
	if stripes > targetRPS {
		stripes = targetRPS
	}
	if stripes < 1 {
		stripes = 1
	}
	perStripeRate := rate.Limit(float64(targetRPS) / float64(stripes))
	totalBurst := targetRPS / 10
	if totalBurst < 1 {
		totalBurst = 1
	}
	perStripeBurst := totalBurst / stripes
	if perStripeBurst < 1 {
		perStripeBurst = 1
	}
	limiters := make([]*rate.Limiter, stripes)
	for i := range limiters {
		limiters[i] = rate.NewLimiter(perStripeRate, perStripeBurst)
	}
	return limiters
}

// Run executes the prober loop until cfg.Duration elapses or ctx is cancelled.
// The returned Result is always non-zero; check Result.Failed for error counts.
func (g *Generator) Run(ctx context.Context) Result {
	runCtx, cancel := context.WithTimeout(ctx, g.cfg.Duration)
	defer cancel()

	collector := newCollector()
	collector.start = time.Now()

	var wg sync.WaitGroup
	for i := 0; i < g.cfg.Concurrency; i++ {
		wg.Add(1)
		// Round-robin workers across the stripes. The modulo ensures
		// every stripe is exercised even when Concurrency < stripes.
		go g.worker(runCtx, &wg, collector, g.limiters[i%len(g.limiters)])
	}
	wg.Wait()

	collector.end = time.Now()
	return collector.finalize(g.cfg)
}

func (g *Generator) worker(ctx context.Context, wg *sync.WaitGroup, c *collector, limiter *rate.Limiter) {
	defer wg.Done()
	for {
		if err := limiter.Wait(ctx); err != nil {
			return // ctx done
		}
		g.do(ctx, c)
	}
}

func (g *Generator) do(ctx context.Context, c *collector) {
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	req.SetRequestURI(g.cfg.URL)
	req.Header.SetMethod(g.cfg.Method)
	for k, v := range g.cfg.Headers {
		req.Header.Set(k, v)
	}
	if g.cfg.ConnectionMode == ShortLived {
		req.Header.SetConnectionClose()
	}

	start := time.Now()
	err := g.client.DoTimeout(req, resp, g.cfg.Timeout)
	latency := time.Since(start)
	at := time.Now()

	if ctx.Err() != nil {
		// Don't record requests that completed after cancellation —
		// they distort the steady-state numbers.
		return
	}

	if err != nil {
		c.recordError(classify(err), latency, at)
		return
	}
	c.recordStatus(resp.StatusCode(), latency, at)
}

// classify maps a fasthttp error to an ErrorCategory.
func classify(err error) ErrorCategory {
	if err == nil {
		return ErrOther
	}
	if errors.Is(err, fasthttp.ErrTimeout) || errors.Is(err, fasthttp.ErrDialTimeout) {
		return ErrTimeout
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "connection reset"):
		return ErrConnReset
	case strings.Contains(msg, "tls:"), strings.Contains(msg, "x509:"):
		return ErrTLS
	case strings.Contains(msg, "dial"), strings.Contains(msg, "no such host"):
		return ErrDial
	}
	return ErrOther
}

// collector aggregates per-request outcomes under a mutex. The Generator
// uses a single collector across all workers.
type collector struct {
	mu sync.Mutex

	start, end time.Time

	sent      int64
	succeeded int64
	failed    int64

	statusCounts map[int]int64
	errors       map[ErrorCategory]int64
	// latencies is an HDR Histogram with fixed (few-KB) footprint, so a
	// long high-RPS run cannot drive the Job pod into OOM by accumulating
	// per-request time.Duration entries in an unbounded slice.
	latencies    *hdrhistogram.Histogram
	errorSamples []ErrorSample
}

func newCollector() *collector {
	return &collector{
		statusCounts: make(map[int]int64),
		errors:       make(map[ErrorCategory]int64),
		latencies:    hdrhistogram.New(latencyHistogramMin, latencyHistogramMax, latencyHistogramSigFig),
	}
}

func (c *collector) recordStatus(code int, latency time.Duration, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent++
	c.statusCounts[code]++
	c.recordLatency(latency)
	switch {
	case code >= 500:
		c.errors[ErrServer]++
		c.failed++
		c.addSample(ErrServer, code, at)
	case code >= 400:
		c.errors[ErrClient]++
		c.failed++
		c.addSample(ErrClient, code, at)
	default:
		c.succeeded++
	}
}

func (c *collector) recordError(cat ErrorCategory, latency time.Duration, at time.Time) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent++
	c.failed++
	c.errors[cat]++
	c.recordLatency(latency)
	c.addSample(cat, 0, at)
}

// recordLatency clamps the value into the histogram range and records it.
// The caller must hold c.mu. Out-of-range values (which would otherwise
// drop silently with an error) are clipped so the percentile tails stay
// honest — a 90s timeout shows up as latencyHistogramMax rather than
// vanishing from the distribution entirely.
func (c *collector) recordLatency(latency time.Duration) {
	v := int64(latency)
	if v < latencyHistogramMin {
		v = latencyHistogramMin
	}
	if v > latencyHistogramMax {
		v = latencyHistogramMax
	}
	_ = c.latencies.RecordValue(v)
}

// addSample appends a failure sample. The caller must hold c.mu. Samples
// past maxErrorSamples are dropped to bound the JSON Result size.
func (c *collector) addSample(cat ErrorCategory, status int, at time.Time) {
	if len(c.errorSamples) >= maxErrorSamples {
		return
	}
	c.errorSamples = append(c.errorSamples, ErrorSample{At: at, Category: cat, Status: status})
}

func (c *collector) finalize(cfg Config) Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	p50 := time.Duration(c.latencies.ValueAtQuantile(50))
	p95 := time.Duration(c.latencies.ValueAtQuantile(95))
	p99 := time.Duration(c.latencies.ValueAtQuantile(99))
	max := time.Duration(c.latencies.Max())
	if c.latencies.TotalCount() == 0 {
		// hdrhistogram's Max() returns its internal minimum sentinel on
		// an empty histogram; force zeros so the JSON Result is honest.
		p50, p95, p99, max = 0, 0, 0, 0
	}
	labels := map[string]string{
		"connectionMode": string(cfg.ConnectionMode),
	}
	if cfg.TargetMode != "" {
		labels["targetMode"] = string(cfg.TargetMode)
	}
	if cfg.NetworkPath != "" {
		labels["networkPath"] = string(cfg.NetworkPath)
	}
	return Result{
		Started:      c.start,
		Ended:        c.end,
		Duration:     c.end.Sub(c.start),
		Sent:         c.sent,
		Succeeded:    c.succeeded,
		Failed:       c.failed,
		StatusCounts: c.statusCounts,
		Errors:       c.errors,
		LatencyP50:   p50,
		LatencyP95:   p95,
		LatencyP99:   p99,
		LatencyMax:   max,
		Labels:       labels,
		ErrorSamples: c.errorSamples,
	}
}
