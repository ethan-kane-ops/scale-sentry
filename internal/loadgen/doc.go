// Package loadgen implements a high-throughput HTTP load generator built on
// fasthttp. It is invoked by the controller as a Kubernetes Job to drive
// traffic at a target deployment while the validation suite runs.
//
// The generator is intentionally narrow:
//   - one URL per run (path isolation, no interleaved traffic patterns)
//   - one connection mode per run (KeepAlive or ShortLived)
//   - rate-limited concurrent prober loop with bounded worker pool
//
// URL construction is exposed via [URLSpec] so callers can build target URLs
// for the ServiceDefault/CustomPath × ClusterIP/Ingress matrix without
// depending on the Kubernetes API.
package loadgen
