// Package leakage implements the cold-start leakage detector. It
// correlates HTTP 5xx / connection-drop samples from the load generator
// with the exact timestamps Pod IPs are plumbed into EndpointSlices.
//
// The diagnostic is "your service started accepting traffic before it was
// actually ready" — requests that fail within a short window after an
// endpoint flips Ready=true indicate that either the readiness probe is
// lying or kube-proxy is racing the endpoint plumbing.
//
// Both event streams are caller-supplied. The controller watches
// discovery.k8s.io/v1.EndpointSlice and converts updates into
// [EndpointEvent] values; loadgen errors are converted to [ErrorSample]
// values from the timestamped error stream the controller collects
// during the run.
package leakage

import (
	"fmt"
	"sort"
	"time"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
)

// EventKind distinguishes endpoint lifecycle transitions.
type EventKind string

const (
	// EndpointReady means the EndpointSlice update transitioned the
	// endpoint's conditions.ready to true (the pod IP just became routable).
	EndpointReady EventKind = "Ready"
	// EndpointRemoved means the endpoint was removed from the slice
	// (pod terminating).
	EndpointRemoved EventKind = "Removed"
)

// EndpointEvent is a single endpoint-state transition observed by the
// controller's EndpointSlice informer.
type EndpointEvent struct {
	At    time.Time
	PodIP string
	Kind  EventKind
}

// ErrorSample is a single loadgen failure with its absolute timestamp.
// Category mirrors loadgen.ErrorCategory; Status is the HTTP status when
// the failure was an HTTP response (4xx/5xx) and 0 otherwise.
type ErrorSample struct {
	At       time.Time
	Category string
	Status   int
}

// DefaultLeakageWindow is the time after an EndpointReady event during
// which a failed request counts as cold-start leakage.
const DefaultLeakageWindow = 2 * time.Second

// CorrelatedEvent pairs a Ready event with the failed requests that
// followed it within the leakage window.
type CorrelatedEvent struct {
	Event  EndpointEvent
	Errors []ErrorSample
}

// Report is the output of [Correlate].
type Report struct {
	Window        time.Duration
	LeakageWindow time.Duration

	// LeakedRequests is the count of failed requests that fell inside
	// the leakage window of at least one EndpointReady event.
	LeakedRequests int
	// CleanRequests is the count of failed requests that fell outside
	// every leakage window — these failures are not cold-start related.
	CleanRequests int
	// Correlated is the per-event breakdown, ordered by event timestamp.
	Correlated []CorrelatedEvent

	// EndpointEventCount is the total EndpointReady events observed.
	EndpointEventCount int
}

// Severity thresholds for [Report.Diagnostics]: any leakage is a Warning,
// >= criticalLeakedCount is a Critical.
const criticalLeakedCount = 50

// Correlate finds, for each EndpointReady event, the failed requests that
// landed within leakageWindow of the event. Errors that match no event
// are tallied in CleanRequests. Both slices may be unsorted; Correlate
// sorts internally and does not mutate the inputs.
//
// If leakageWindow <= 0, [DefaultLeakageWindow] is used.
func Correlate(events []EndpointEvent, errors []ErrorSample, leakageWindow time.Duration) Report {
	if leakageWindow <= 0 {
		leakageWindow = DefaultLeakageWindow
	}

	// Copy + sort so we can binary-search and not mutate caller slices.
	readyEvents := make([]EndpointEvent, 0, len(events))
	for _, e := range events {
		if e.Kind == EndpointReady {
			readyEvents = append(readyEvents, e)
		}
	}
	sort.Slice(readyEvents, func(i, j int) bool {
		return readyEvents[i].At.Before(readyEvents[j].At)
	})

	sortedErrors := make([]ErrorSample, len(errors))
	copy(sortedErrors, errors)
	sort.Slice(sortedErrors, func(i, j int) bool {
		return sortedErrors[i].At.Before(sortedErrors[j].At)
	})

	r := Report{
		LeakageWindow:      leakageWindow,
		EndpointEventCount: len(readyEvents),
	}
	if len(readyEvents) == 0 {
		r.CleanRequests = len(sortedErrors)
		r.Window = totalWindow(sortedErrors)
		return r
	}

	correlated := make([]CorrelatedEvent, len(readyEvents))
	for i, ev := range readyEvents {
		correlated[i] = CorrelatedEvent{Event: ev}
	}

	for _, errSample := range sortedErrors {
		assigned := false
		// An error is "leaked" if it falls in any [event.At, event.At + window).
		for i, ev := range readyEvents {
			if errSample.At.Before(ev.At) {
				continue
			}
			if errSample.At.Sub(ev.At) >= leakageWindow {
				continue
			}
			correlated[i].Errors = append(correlated[i].Errors, errSample)
			assigned = true
			break
		}
		if assigned {
			r.LeakedRequests++
		} else {
			r.CleanRequests++
		}
	}

	r.Correlated = correlated
	r.Window = totalWindow(sortedErrors)
	return r
}

func totalWindow(samples []ErrorSample) time.Duration {
	if len(samples) < 2 {
		return 0
	}
	return samples[len(samples)-1].At.Sub(samples[0].At)
}

// Diagnostics emits a ColdStartLeakage alert when leaked requests were
// observed. Severity escalates to Critical past criticalLeakedCount.
func (r Report) Diagnostics() []v1alpha1.DiagnosticAlert {
	if r.LeakedRequests == 0 {
		return nil
	}
	severity := "Warning"
	if r.LeakedRequests >= criticalLeakedCount {
		severity = "Critical"
	}
	return []v1alpha1.DiagnosticAlert{{
		Type:     "ColdStartLeakage",
		Severity: severity,
		Message: fmt.Sprintf(
			"%d failed requests landed within %s of an endpoint becoming Ready (across %d ready events)",
			r.LeakedRequests, r.LeakageWindow, r.EndpointEventCount,
		),
		Recommendation: "add readinessProbe.initialDelaySeconds or a startupProbe; verify the application returns 503 until warmup completes; check kube-proxy iptables sync latency",
	}}
}
