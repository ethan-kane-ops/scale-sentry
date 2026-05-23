package loadgen

import (
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

// ErrorSample is a single failed request tagged with the wall-clock time
// it completed. The observer converts these into leakage.ErrorSample to
// correlate failures against endpoint-readiness and pod-drain events.
type ErrorSample struct {
	At       time.Time     `json:"at"`
	Category ErrorCategory `json:"category"`
	// Status is the HTTP status code for 4xx/5xx failures, 0 for
	// transport-level errors (timeout, dial, reset, TLS).
	Status int `json:"status"`
}

// maxErrorSamples caps Result.ErrorSamples so a pathological run cannot
// produce an unbounded JSON payload. A run with more failures than this is
// already a clear failure — truncation does not change the verdict.
const maxErrorSamples = 10000

// Result is the summary emitted by [Generator.Run].
type Result struct {
	Started      time.Time               `json:"started"`
	Ended        time.Time               `json:"ended"`
	Duration     time.Duration           `json:"duration"`
	Sent         int64                   `json:"sent"`
	Succeeded    int64                   `json:"succeeded"`
	Failed       int64                   `json:"failed"`
	StatusCounts map[int]int64           `json:"statusCounts"`
	Errors       map[ErrorCategory]int64 `json:"errors"`
	LatencyP50   time.Duration           `json:"latencyP50"`
	LatencyP95   time.Duration           `json:"latencyP95"`
	LatencyP99   time.Duration           `json:"latencyP99"`
	LatencyMax   time.Duration           `json:"latencyMax"`
	Labels       map[string]string       `json:"labels"`
	// ErrorSamples holds one timestamped entry per failed request, capped
	// at maxErrorSamples. Empty when the run had no failures.
	ErrorSamples []ErrorSample `json:"errorSamples,omitempty"`
}

// FailureRate returns Failed / Sent in [0, 1]. Returns 0 when no requests sent.
func (r Result) FailureRate() float64 {
	if r.Sent == 0 {
		return 0
	}
	return float64(r.Failed) / float64(r.Sent)
}

