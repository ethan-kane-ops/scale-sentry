// Package probelag implements the Readiness Probe Sampling Lag Analyzer.
// It compares the canonical Pod lifecycle conditions (PodScheduled →
// Initialized → ContainersReady → PodReady) to surface two failure modes:
//
//   - **Sparse sampling**, the probe didn't sample often enough. A long
//     gap between ContainersReady and PodReady relative to periodSeconds
//     means the kubelet waited multiple probe periods before declaring the
//     pod ready, prolonging cold-start traffic exposure.
//   - **Container startup latency**, Initialized → ContainersReady is the
//     time the container takes to come up; surfaced informationally.
//
// The package never queries the K8s API. The controller fetches
// pod.status.conditions and passes the slice in.
package probelag

import (
	"fmt"
	"time"

	corev1 "k8s.io/api/core/v1"

	v1beta1 "github.com/ethan-kane-ops/scale-sentry/api/v1beta1"
)

// Report holds the absolute condition timestamps and the derived latencies.
// Timestamp pointers are nil when the corresponding condition is absent or
// has status != True.
type Report struct {
	Scheduled              *time.Time
	ReadyToStartContainers *time.Time
	Initialized            *time.Time
	ContainersReady        *time.Time
	Ready                  *time.Time

	// SchedulingLatency is Scheduled → Initialized (kept for backwards
	// compatibility; equals TrueSchedulingLatency + InitContainerRuntime
	// when the kubelet exposes PodReadyToStartContainers).
	SchedulingLatency time.Duration
	// TrueSchedulingLatency is Scheduled → PodReadyToStartContainers, the
	// kube-scheduler bind + kubelet sandbox setup phase. Zero when the
	// PodReadyToStartContainers condition is absent (clusters older than
	// the GA of the condition or where the feature gate is off).
	TrueSchedulingLatency time.Duration
	// InitContainerRuntime is PodReadyToStartContainers → Initialized, // init-container image pull + execution time, which previously rolled
	// up into SchedulingLatency and inflated it whenever a workload used
	// non-trivial init containers (sidecar bootstraps, schema migrations).
	// Zero when PodReadyToStartContainers is absent.
	InitContainerRuntime time.Duration
	// StartupLatency is Initialized → ContainersReady (container image pull,
	// process start, dependencies).
	StartupLatency time.Duration
	// ReadinessLag is ContainersReady → PodReady, the gap where the
	// container is running but the readiness probe has not yet confirmed it.
	// Long lags indicate the kubelet is sampling sparsely relative to
	// periodSeconds.
	ReadinessLag time.Duration
	// TotalStartupLatency is Scheduled → Ready (full cold-start).
	TotalStartupLatency time.Duration

	// PeriodSeconds is the configured readinessProbe.periodSeconds (input).
	// Zero disables the sparse-sampling diagnostic.
	PeriodSeconds int32
}

// Severity thresholds for [Report.Diagnostics], expressed as multiples of
// PeriodSeconds. A ReadinessLag of >= warnPeriods periods is a Warning;
// >= criticalPeriods is Critical.
const (
	warnPeriods     = 2
	criticalPeriods = 5
)

// FromPodConditions builds a Report from a pod's conditions slice and the
// probe's periodSeconds. Only conditions with status == True populate a
// timestamp; absent or False conditions leave the pointer nil.
func FromPodConditions(conds []corev1.PodCondition, periodSeconds int32) Report {
	r := Report{PeriodSeconds: periodSeconds}

	for _, c := range conds {
		if c.Status != corev1.ConditionTrue {
			continue
		}
		ts := c.LastTransitionTime.Time
		switch c.Type {
		case corev1.PodScheduled:
			r.Scheduled = &ts
		case corev1.PodReadyToStartContainers:
			r.ReadyToStartContainers = &ts
		case corev1.PodInitialized:
			r.Initialized = &ts
		case corev1.ContainersReady:
			r.ContainersReady = &ts
		case corev1.PodReady:
			r.Ready = &ts
		}
	}

	r.SchedulingLatency = diff(r.Scheduled, r.Initialized)
	// Only populate the split when the kubelet exposed
	// PodReadyToStartContainers (kubernetes >=1.29 GA, gated earlier).
	// Without that timestamp the boundary between scheduler/sandbox and
	// init-container work is unknowable, so both halves stay zero and
	// SchedulingLatency remains the only number consumers can trust.
	if r.ReadyToStartContainers != nil {
		r.TrueSchedulingLatency = diff(r.Scheduled, r.ReadyToStartContainers)
		r.InitContainerRuntime = diff(r.ReadyToStartContainers, r.Initialized)
	}
	r.StartupLatency = diff(r.Initialized, r.ContainersReady)
	r.ReadinessLag = diff(r.ContainersReady, r.Ready)
	r.TotalStartupLatency = diff(r.Scheduled, r.Ready)
	return r
}

func diff(a, b *time.Time) time.Duration {
	if a == nil || b == nil {
		return 0
	}
	d := b.Sub(*a)
	if d < 0 {
		return 0
	}
	return d
}

// Diagnostics emits a single ReadinessSamplingSparse alert when the
// readiness lag is multiple probe periods long. Severity tracks how many
// periods the lag covered.
func (r Report) Diagnostics() []v1beta1.DiagnosticAlert {
	if r.PeriodSeconds <= 0 {
		return nil
	}
	if r.ContainersReady == nil || r.Ready == nil {
		return nil
	}
	period := time.Duration(r.PeriodSeconds) * time.Second
	if r.ReadinessLag < time.Duration(warnPeriods)*period {
		return nil
	}
	severity := v1beta1.SeverityWarning
	if r.ReadinessLag >= time.Duration(criticalPeriods)*period {
		severity = v1beta1.SeverityCritical
	}
	periodsCovered := float64(r.ReadinessLag) / float64(period)
	return []v1beta1.DiagnosticAlert{{
		Type:     "ReadinessSamplingSparse",
		Severity: severity,
		Message: fmt.Sprintf(
			"readiness lag %s spans %.1fx the probe period (%s), container was running but unmarked Ready for %d+ probe cycles",
			r.ReadinessLag, periodsCovered, period, int(periodsCovered),
		),
		Recommendation: "lower readinessProbe.periodSeconds (e.g. 2s) and/or readinessProbe.failureThreshold to shorten cold-start exposure",
	}}
}
