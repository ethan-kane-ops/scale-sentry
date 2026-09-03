// Package cgroup parses cgroup v2 `cpu.stat` files and computes CFS
// throttling percentages over a measurement window.
//
// Format reference (kernel docs):
//
//	usage_usec 12345678
//	user_usec  9123456
//	system_usec 3222222
//	nr_periods 100
//	nr_throttled 7
//	throttled_usec 42000
//
// The controller scrapes the file inside the target pod's container at the
// start and end of the active stress window, then calls [Compare] to derive
// the percentage of CFS periods that hit the quota ceiling.
package cgroup

import (
	"bufio"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	v1beta1 "github.com/ethan-kane-ops/scale-sentry/api/v1beta1"
)

// Stat is the parsed contents of a cgroup v2 `cpu.stat` file. Unknown
// fields are ignored; missing fields default to zero.
type Stat struct {
	UsageUSec     uint64
	UserUSec      uint64
	SystemUSec    uint64
	NRPeriods     uint64
	NRThrottled   uint64
	ThrottledUSec uint64
}

// Parse reads a `cpu.stat` body and returns the populated [Stat].
// Returns an error if a recognised key has a non-numeric value.
func Parse(r io.Reader) (Stat, error) {
	var s Stat
	scanner := bufio.NewScanner(r)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" {
			continue
		}
		key, raw, ok := strings.Cut(line, " ")
		if !ok {
			return Stat{}, fmt.Errorf("malformed line %q", line)
		}
		val, err := strconv.ParseUint(strings.TrimSpace(raw), 10, 64)
		if err != nil {
			return Stat{}, fmt.Errorf("parse %s value %q: %w", key, raw, err)
		}
		switch key {
		case "usage_usec":
			s.UsageUSec = val
		case "user_usec":
			s.UserUSec = val
		case "system_usec":
			s.SystemUSec = val
		case "nr_periods":
			s.NRPeriods = val
		case "nr_throttled":
			s.NRThrottled = val
		case "throttled_usec":
			s.ThrottledUSec = val
		}
	}
	if err := scanner.Err(); err != nil {
		return Stat{}, fmt.Errorf("scan cpu.stat: %w", err)
	}
	return s, nil
}

// ParseFile is a convenience wrapper around [Parse] for filesystem paths.
func ParseFile(path string) (Stat, error) {
	f, err := os.Open(path)
	if err != nil {
		return Stat{}, fmt.Errorf("open cpu.stat: %w", err)
	}
	defer func() { _ = f.Close() }()
	return Parse(f)
}

// Report is the delta between two [Stat] samples plus derived metrics.
type Report struct {
	Before, After Stat
	Window        time.Duration

	// Periods is the number of CFS scheduling periods that elapsed in Window.
	Periods uint64
	// Throttled is the number of those periods where the cgroup hit its CPU quota.
	Throttled uint64
	// ThrottlePercent is Throttled / Periods × 100. Zero when Periods is 0.
	ThrottlePercent float64
	// ThrottledDuration is the cumulative wall-clock time spent throttled in Window.
	ThrottledDuration time.Duration
}

// Compare derives the throttling report from two consecutive samples and the
// observed window between them. Counters are unsigned-monotonic and underflow
// is treated as zero (defensive, should never happen for healthy kernels).
func Compare(before, after Stat, window time.Duration) Report {
	r := Report{Before: before, After: after, Window: window}
	r.Periods = saturatingSub(after.NRPeriods, before.NRPeriods)
	r.Throttled = saturatingSub(after.NRThrottled, before.NRThrottled)
	throttledUSec := saturatingSub(after.ThrottledUSec, before.ThrottledUSec)
	r.ThrottledDuration = time.Duration(throttledUSec) * time.Microsecond
	if r.Periods > 0 {
		r.ThrottlePercent = float64(r.Throttled) / float64(r.Periods) * 100.0
	}
	return r
}

func saturatingSub(a, b uint64) uint64 {
	if a < b {
		return 0
	}
	return a - b
}

// Severity thresholds for [Report.Diagnostics]. Tuned to match the plan's
// alerting guidance: any throttling is informational, >5% warns, >25% is critical.
const (
	WarnThresholdPercent     = 5.0
	CriticalThresholdPercent = 25.0
)

// Diagnostics converts the report into zero or one [v1beta1.DiagnosticAlert].
// Returns nil when no throttling was observed.
func (r Report) Diagnostics() []v1beta1.DiagnosticAlert {
	if r.Throttled == 0 || r.Periods == 0 {
		return nil
	}
	severity := v1beta1.SeverityInfo
	switch {
	case r.ThrottlePercent >= CriticalThresholdPercent:
		severity = v1beta1.SeverityCritical
	case r.ThrottlePercent >= WarnThresholdPercent:
		severity = v1beta1.SeverityWarning
	}
	return []v1beta1.DiagnosticAlert{{
		Type:     "CPUThrottling",
		Severity: severity,
		Message: fmt.Sprintf(
			"container throttled %d of %d CFS periods (%.2f%%) for %s during the stress window",
			r.Throttled, r.Periods, r.ThrottlePercent, r.ThrottledDuration,
		),
		Recommendation: "raise cpu.limits or remove the limit; CPU limits force CFS throttling even when the node has spare capacity",
	}}
}
