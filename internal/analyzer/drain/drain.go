// Package drain detects ungraceful pod shutdown. It correlates HTTP
// failures from the load generator with the timestamps pod endpoints are
// removed from EndpointSlices.
//
// Errors that land around an endpoint removal indicate the pod stopped
// accepting connections without draining in-flight requests, typically a
// missing preStop hook, too-short terminationGracePeriodSeconds, or a
// SIGTERM that propagated faster than kube-proxy could update iptables. The
// removal timestamp itself is the observer's informer-watch receive time,
// not the moment the pod actually stopped serving, so a real drop can be
// stamped slightly *before* the removal it belongs to; see DefaultLookback.
//
// drain reuses leakage.EndpointEvent and leakage.ErrorSample on purpose:
// the controller produces exactly one endpoint-event stream and one
// loadgen-error stream per run, and both this package and leakage consume
// them. drain looks at the Removed side; leakage looks at the Ready side.
package drain

import (
	"fmt"
	"sort"
	"time"

	v1beta1 "github.com/ethan-kane-ops/scale-sentry/api/v1beta1"
	"github.com/ethan-kane-ops/scale-sentry/internal/analyzer/leakage"
)

// DefaultDrainWindow is the time after an endpoint removal during which a
// failed request counts as an ungraceful-drain drop. A graceful shutdown
// produces no new failures once the endpoint is gone.
const DefaultDrainWindow = 10 * time.Second

// DefaultLookback is the time before an endpoint removal during which a
// failed request still counts as a drop attributed to that removal.
// Endpoint event timestamps are recorded when the observer's informer
// watch receives them, which trails the pod actually going unready by
// anywhere from a few ms (warm watch) to several hundred (cold watch /
// busy apiserver) — see internal/observer's MetricsLikelySkewed
// diagnostic. Without this tolerance, the fastest, most-ungraceful drops
// (the ones this analyzer exists to catch) land just before the delayed
// removal timestamp and get misclassified as unrelated/clean.
const DefaultLookback = 2 * time.Second

// criticalDroppedCount is the drop count above which the diagnostic
// escalates from Warning to Critical.
const criticalDroppedCount = 25

// CorrelatedRemoval pairs an endpoint removal with the failed requests
// that followed it inside the drain window.
type CorrelatedRemoval struct {
	Event  leakage.EndpointEvent
	Errors []leakage.ErrorSample
}

// Report is the outcome of [Correlate].
type Report struct {
	DrainWindow time.Duration
	Window      time.Duration

	// DroppedRequests is the count of failures inside the drain window of
	// at least one removal, requests the pod failed to drain gracefully.
	DroppedRequests int
	// CleanRequests is the count of failures outside every drain window.
	CleanRequests int
	// Correlated is the per-removal breakdown, ordered by removal time.
	Correlated []CorrelatedRemoval
	// RemovalCount is the number of EndpointRemoved events observed.
	RemovalCount int
}

// Correlate finds, for each EndpointRemoved event, the failed requests that
// landed within drainWindow of the removal. Inputs may be unsorted;
// Correlate sorts copies and does not mutate the caller's slices.
//
// A drainWindow <= 0 uses [DefaultDrainWindow].
func Correlate(events []leakage.EndpointEvent, errors []leakage.ErrorSample, drainWindow time.Duration) Report {
	if drainWindow <= 0 {
		drainWindow = DefaultDrainWindow
	}

	removals := make([]leakage.EndpointEvent, 0, len(events))
	for _, e := range events {
		if e.Kind == leakage.EndpointRemoved {
			removals = append(removals, e)
		}
	}
	sort.Slice(removals, func(i, j int) bool { return removals[i].At.Before(removals[j].At) })

	sorted := make([]leakage.ErrorSample, len(errors))
	copy(sorted, errors)
	sort.Slice(sorted, func(i, j int) bool { return sorted[i].At.Before(sorted[j].At) })

	r := Report{DrainWindow: drainWindow, RemovalCount: len(removals)}
	if len(removals) == 0 {
		r.CleanRequests = len(sorted)
		r.Window = totalWindow(sorted)
		return r
	}

	correlated := make([]CorrelatedRemoval, len(removals))
	for i, ev := range removals {
		correlated[i] = CorrelatedRemoval{Event: ev}
	}

	// Two-pointer scan over sorted removals + sorted errors, same
	// rationale as the leakage analyzer: both streams move forward in
	// time, so a closed drain window cannot re-open for later errors,
	// turning the old O(R*E) match into O(R + E). Widening the window's
	// start by DefaultLookback keeps removal windows' bounds monotonic
	// (each removal is later than the last, so its lookback-adjusted
	// start is too), so the scan stays forward-only.
	j := 0
	for _, es := range sorted {
		for j < len(removals) && !es.At.Before(removals[j].At.Add(drainWindow)) {
			j++
		}
		if j >= len(removals) || es.At.Before(removals[j].At.Add(-DefaultLookback)) {
			r.CleanRequests++
			continue
		}
		correlated[j].Errors = append(correlated[j].Errors, es)
		r.DroppedRequests++
	}

	r.Correlated = correlated
	r.Window = totalWindow(sorted)
	return r
}

func totalWindow(samples []leakage.ErrorSample) time.Duration {
	if len(samples) < 2 {
		return 0
	}
	return samples[len(samples)-1].At.Sub(samples[0].At)
}

// Diagnostics emits an UngracefulDrain alert when dropped requests were
// observed. Severity escalates to Critical past criticalDroppedCount.
func (r Report) Diagnostics() []v1beta1.DiagnosticAlert {
	if r.DroppedRequests == 0 {
		return nil
	}
	severity := v1beta1.SeverityWarning
	if r.DroppedRequests >= criticalDroppedCount {
		severity = v1beta1.SeverityCritical
	}
	return []v1beta1.DiagnosticAlert{{
		Type:     "UngracefulDrain",
		Severity: severity,
		Message: fmt.Sprintf(
			"%d requests failed within %s of a pod endpoint being removed (across %d removals)",
			r.DroppedRequests, r.DrainWindow, r.RemovalCount),
		Recommendation: "add a preStop hook that sleeps past the kube-proxy sync interval, raise terminationGracePeriodSeconds, and ensure the app keeps draining in-flight requests after SIGTERM",
	}}
}
