package loadgen

import (
	"sort"
	"time"
)

// ErrorCategory classifies a failed request.
type ErrorCategory string

const (
	ErrTimeout   ErrorCategory = "Timeout"
	ErrDial      ErrorCategory = "Dial"
	ErrConnReset ErrorCategory = "ConnReset"
	ErrTLS       ErrorCategory = "TLS"
	ErrServer    ErrorCategory = "Server5xx"
	ErrClient    ErrorCategory = "Client4xx"
	ErrOther     ErrorCategory = "Other"
)

// Result is the summary emitted by [Generator.Run].
type Result struct {
	Started      time.Time          `json:"started"`
	Ended        time.Time          `json:"ended"`
	Duration     time.Duration      `json:"duration"`
	Sent         int64              `json:"sent"`
	Succeeded    int64              `json:"succeeded"`
	Failed       int64              `json:"failed"`
	StatusCounts map[int]int64      `json:"statusCounts"`
	Errors       map[ErrorCategory]int64 `json:"errors"`
	LatencyP50   time.Duration      `json:"latencyP50"`
	LatencyP95   time.Duration      `json:"latencyP95"`
	LatencyP99   time.Duration      `json:"latencyP99"`
	LatencyMax   time.Duration      `json:"latencyMax"`
	Labels       map[string]string  `json:"labels"`
}

// FailureRate returns Failed / Sent in [0, 1]. Returns 0 when no requests sent.
func (r Result) FailureRate() float64 {
	if r.Sent == 0 {
		return 0
	}
	return float64(r.Failed) / float64(r.Sent)
}

// percentiles computes p50/p95/p99/max from a slice of latencies.
// The slice is sorted in place. Returns zero values when samples is empty.
func percentiles(samples []time.Duration) (p50, p95, p99, max time.Duration) {
	if len(samples) == 0 {
		return 0, 0, 0, 0
	}
	sort.Slice(samples, func(i, j int) bool { return samples[i] < samples[j] })
	pick := func(q float64) time.Duration {
		// nearest-rank percentile
		idx := int(float64(len(samples)-1) * q)
		return samples[idx]
	}
	return pick(0.50), pick(0.95), pick(0.99), samples[len(samples)-1]
}
