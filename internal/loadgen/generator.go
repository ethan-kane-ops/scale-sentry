package loadgen

import (
	"context"
	"fmt"
	"sync"
	"time"

	hdrhistogram "github.com/HdrHistogram/hdrhistogram-go"
)

// Latency histogram bounds. 1µs..60s with 3 significant figures matches
// the operational range scale-sentry runs in (sub-ms hot paths through to
// timeouts) and keeps the per-histogram footprint to a few KB, orders of
// magnitude smaller than an unbounded []time.Duration that grew to 50M+
// entries on long high-RPS runs and dominated the Job pod's memory.
const (
	latencyHistogramMin    = int64(time.Microsecond / time.Nanosecond)
	latencyHistogramMax    = int64(60 * time.Second / time.Nanosecond)
	latencyHistogramSigFig = 3
)

// Generator drives a single concurrent prober run. One Generator runs
// exactly one URL with one ConnectionMode and one Protocol; re-use
// across runs is not supported.
type Generator struct {
	cfg    Config
	client protocolClient
}

// New constructs a Generator. Validates cfg and returns any error encountered.
func New(cfg Config) (*Generator, error) {
	if err := cfg.Validate(); err != nil {
		return nil, err
	}
	cfg = cfg.withDefaults()
	client, err := newProtocolClient(cfg)
	if err != nil {
		return nil, fmt.Errorf("build client: %w", err)
	}
	return &Generator{
		cfg:    cfg,
		client: client,
	}, nil
}

// Run executes each phase of cfg.Phases in order, then returns the
// aggregated Result. Phases with RecordStats=false are dispatched (so
// the target's caches warm, TLS handshakes settle, JIT happens) but
// their requests are excluded from the histogram, status counts, and
// verdict totals.
//
// Latency is measured from each request's *scheduled* arrival time, not
// the moment the worker actually dispatched it. This is the
// coordinated-omission fix: when the target stalls, scheduled tokens
// pile up in the channel, and the latency reflects the full
// user-visible delay (queue wait + response time), not just the network
// roundtrip after dispatch.
func (g *Generator) Run(ctx context.Context) Result {
	collector := newCollector()
	collector.start = time.Now()

	for i := range g.cfg.Phases {
		if ctx.Err() != nil {
			break
		}
		g.runPhase(ctx, &g.cfg.Phases[i], collector)
	}

	collector.end = time.Now()
	collector.clientStats = g.client.Stats()
	return collector.finalize(g.cfg)
}

// runPhase dispatches one phase's worth of traffic against the target.
// A dedicated emitter goroutine generates the scheduled-arrival
// timestamps for the phase; a fixed-size worker pool sends a request per
// timestamp. The phase ends when its Duration elapses (emitter closes
// the channel) and all in-flight workers drain.
func (g *Generator) runPhase(ctx context.Context, phase *Phase, c *collector) {
	phaseCtx, cancel := context.WithTimeout(ctx, phase.Duration)
	defer cancel()

	c.beginPhase(phase)
	defer c.endPhase(phase)

	ch := make(chan scheduledArrival, scheduleBuffer)
	go runSchedule(phaseCtx, time.Now(), *phase, ch)

	var wg sync.WaitGroup
	for i := 0; i < g.cfg.Concurrency; i++ {
		wg.Add(1)
		go g.worker(phaseCtx, &wg, c, phase, ch)
	}
	wg.Wait()
}

// worker pulls scheduled arrivals from ch and dispatches a request per
// arrival until ch closes or ctx is cancelled. RecordStats=false phases
// run the request but the collector's record paths drop the sample.
func (g *Generator) worker(ctx context.Context, wg *sync.WaitGroup, c *collector, phase *Phase, ch <-chan scheduledArrival) {
	defer wg.Done()
	for {
		select {
		case <-ctx.Done():
			return
		case slot, ok := <-ch:
			if !ok {
				return
			}
			g.do(ctx, c, phase, slot)
		}
	}
}

// do issues one request and records its outcome against c. The latency
// clock starts at slot.Time (the scheduled arrival), so a stalled target
// that backs the workers up will show up as elevated latency rather than
// hidden queue depth (the coordinated-omission fix). The actual wire
// dispatch is delegated to g.client (HTTP/1 via fasthttp or HTTP/2 via
// net/http + http2.Transport).
func (g *Generator) do(ctx context.Context, c *collector, phase *Phase, slot scheduledArrival) {
	// If we're early, hold the request until its scheduled instant. This
	// keeps the arrival distribution honest under low CPU contention; if
	// we're late, fire immediately and the latency captures the lag.
	if d := time.Until(slot.Time); d > 0 {
		select {
		case <-ctx.Done():
			return
		case <-time.After(d):
		}
	}

	status, err := g.client.Do(ctx)
	completion := time.Now()
	latency := completion.Sub(slot.Time)
	if latency < 0 {
		// Defensive: a clock skew or scheduled-in-the-past slot would
		// otherwise feed negative values into the histogram.
		latency = 0
	}

	if ctx.Err() != nil {
		// Don't record requests that completed after cancellation, // they distort the steady-state numbers.
		return
	}
	if !phase.RecordStats {
		// Warmup: send but discard. The target sees real traffic so
		// its caches/JIT warm, but the histogram only retains the
		// measurement window.
		return
	}

	if err != nil {
		c.recordError(classify(err), latency, completion)
		return
	}
	c.recordStatus(status, latency, completion)
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

	// Per-phase summaries written into Result.Phases. Indexed in phase
	// declaration order so callers can correlate against cfg.Phases.
	phases []PhaseSummary

	// clientStats is the final post-run snapshot from the
	// protocolClient (GOAWAY count, streams/conn) attached by Run()
	// before finalize.
	clientStats ClientStats
}

func newCollector() *collector {
	return &collector{
		statusCounts: make(map[int]int64),
		errors:       make(map[ErrorCategory]int64),
		latencies:    hdrhistogram.New(latencyHistogramMin, latencyHistogramMax, latencyHistogramSigFig),
	}
}

// beginPhase appends a new PhaseSummary entry the worker pool fills in.
// The summary's start timestamp is the actual wall-clock dispatch start,
// not the scheduled time, so post-hoc analysis can spot a phase that was
// itself late.
func (c *collector) beginPhase(p *Phase) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.phases = append(c.phases, PhaseSummary{
		Name:        p.Name,
		Pattern:     string(p.Pattern),
		Duration:    p.Duration,
		StartRPS:    p.StartRPS,
		RecordStats: p.RecordStats,
		Started:     time.Now(),
	})
}

// endPhase stamps the trailing wall-clock timestamp on the current phase.
func (c *collector) endPhase(_ *Phase) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if len(c.phases) == 0 {
		return
	}
	c.phases[len(c.phases)-1].Ended = time.Now()
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
// honest, a 90s timeout shows up as latencyHistogramMax rather than
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
		"protocol":       string(cfg.Protocol),
	}
	if cfg.TargetMode != "" {
		labels["targetMode"] = string(cfg.TargetMode)
	}
	if cfg.NetworkPath != "" {
		labels["networkPath"] = string(cfg.NetworkPath)
	}
	if cfg.Protocol == ProtocolHTTP2 {
		labels["goAwayCount"] = fmt.Sprintf("%d", c.clientStats.GoAwayCount)
		labels["connsOpened"] = fmt.Sprintf("%d", c.clientStats.ConnsOpened)
		labels["streamsIssued"] = fmt.Sprintf("%d", c.clientStats.StreamsIssued)
	}
	var warmup, measure time.Duration
	for _, ps := range c.phases {
		if ps.RecordStats {
			measure += ps.Ended.Sub(ps.Started)
		} else {
			warmup += ps.Ended.Sub(ps.Started)
		}
	}
	return Result{
		Started:             c.start,
		Ended:               c.end,
		Duration:            c.end.Sub(c.start),
		WarmupDuration:      warmup,
		MeasurementDuration: measure,
		Sent:                c.sent,
		Succeeded:           c.succeeded,
		Failed:              c.failed,
		StatusCounts:        c.statusCounts,
		Errors:              c.errors,
		LatencyP50:          p50,
		LatencyP95:          p95,
		LatencyP99:          p99,
		LatencyMax:          max,
		Labels:              labels,
		ErrorSamples:        c.errorSamples,
		Phases:              c.phases,
	}
}
