// Package loadgen implements a high-throughput HTTP load generator. By
// default it dispatches via fasthttp (HTTP/1.1); when Config.Protocol
// is set to ProtocolHTTP2 it swaps in a net/http + http2.Transport
// client speaking ALPN-negotiated h2 (TLS) or prior-knowledge h2c
// (cleartext). It is invoked by the controller as a Kubernetes Job to
// drive traffic at a target deployment while the validation suite runs.
//
// The generator is intentionally narrow:
//   - one URL per run (path isolation, no interleaved traffic patterns)
//   - one connection mode per run (KeepAlive or ShortLived)
//   - one wire protocol per run (HTTP1 or HTTP2)
//   - ordered phase list with a bounded scheduled-arrival channel per phase
//
// URL construction is exposed via [URLSpec] so callers can build target URLs
// for the ServiceDefault/CustomPath × ClusterIP/Ingress matrix without
// depending on the Kubernetes API.
//
// # Phases and arrival patterns
//
// A run is one or more [Phase] segments executed in order. Each phase
// carries its own [Pattern] (arrival shape) and rate parameters:
//
//   - [PatternConstant] emits at a fixed rate; the legacy single-shot
//     default.
//   - [PatternPoisson] emits with exponential inter-arrival times of mean
//     1/rate (open-loop). Closer to real user traffic than Constant;
//     SLA verdicts drawn from Poisson runs are more conservative because
//     burstiness shows up in the tail.
//   - [PatternRamp] linearly interpolates rate from StartRPS to EndRPS
//     across the phase Duration. Useful for measuring HPA reaction
//     latency under monotonic load growth.
//   - [PatternStep] climbs in fixed StepRPS increments every StepEvery,
//     holding each plateau as a Constant slice. Stresses HPA
//     stabilization windows.
//
// Spike behaviour is built at the CR/controller layer by decomposing a
// Spike profile into Constant base / Constant spike / ... phases; the
// loadgen package itself only knows the four atomic patterns above.
//
// A phase with RecordStats=false (conventionally named [WarmupPhaseName])
// dispatches requests against the target but excludes them from the
// latency histogram, status counters, and ErrorSamples. This keeps
// TCP/TLS slow-start, JIT, and page-cache misses out of the SLA verdict
// without skipping the warmup pass entirely.
//
// # Scheduled-arrival latency (coordinated-omission fix)
//
// Each phase has a dedicated emitter goroutine that produces
// [scheduledArrival] timestamps at the rate dictated by the phase
// pattern. Workers pull from a small buffered channel and dispatch one
// request per slot. The latency of a recorded sample is measured from
// the slot's *scheduled* time, not the moment the worker actually fired
// the request. So when the target stalls and the slot channel backs up,
// the queue wait shows up in the latency distribution rather than being
// silently absorbed (the canonical "coordinated omission" failure mode
// of closed-loop benchmarks).
package loadgen
