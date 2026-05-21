package loadgen

import (
	"context"
	"errors"
	"strings"
	"sync"
	"time"

	"github.com/valyala/fasthttp"
	"golang.org/x/time/rate"
)

// Generator drives a single concurrent prober run. One Generator runs
// exactly one URL with one ConnectionMode — re-use across runs is not
// supported.
type Generator struct {
	cfg     Config
	client  *fasthttp.Client
	limiter *rate.Limiter
}

// New constructs a Generator. Validates cfg and returns any error encountered.
func New(cfg Config) (*Generator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()
	burst := cfg.TargetRPS / 10
	if burst < 1 {
		burst = 1
	}
	return &Generator{
		cfg:     cfg,
		client:  newClient(cfg.ConnectionMode, cfg.Timeout),
		limiter: rate.NewLimiter(rate.Limit(cfg.TargetRPS), burst),
	}, nil
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
		go g.worker(runCtx, &wg, collector)
	}
	wg.Wait()

	collector.end = time.Now()
	return collector.finalize(g.cfg)
}

func (g *Generator) worker(ctx context.Context, wg *sync.WaitGroup, c *collector) {
	defer wg.Done()
	for {
		if err := g.limiter.Wait(ctx); err != nil {
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

	if ctx.Err() != nil {
		// Don't record requests that completed after cancellation —
		// they distort the steady-state numbers.
		return
	}

	if err != nil {
		c.recordError(classify(err), latency)
		return
	}
	c.recordStatus(resp.StatusCode(), latency)
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
	latencies    []time.Duration
}

func newCollector() *collector {
	return &collector{
		statusCounts: make(map[int]int64),
		errors:       make(map[ErrorCategory]int64),
		latencies:    make([]time.Duration, 0, 1024),
	}
}

func (c *collector) recordStatus(code int, latency time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent++
	c.statusCounts[code]++
	c.latencies = append(c.latencies, latency)
	switch {
	case code >= 500:
		c.errors[ErrServer]++
		c.failed++
	case code >= 400:
		c.errors[ErrClient]++
		c.failed++
	default:
		c.succeeded++
	}
}

func (c *collector) recordError(cat ErrorCategory, latency time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.sent++
	c.failed++
	c.errors[cat]++
	c.latencies = append(c.latencies, latency)
}

func (c *collector) finalize(cfg Config) Result {
	c.mu.Lock()
	defer c.mu.Unlock()
	p50, p95, p99, max := percentiles(c.latencies)
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
	}
}
