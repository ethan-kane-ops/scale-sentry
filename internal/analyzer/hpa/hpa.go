// Package hpa is the HPA reaction-latency / SLA watcher. The controller
// feeds successive [Snapshot] values pulled from the cluster; the watcher
// derives reaction latency (time to first scale event), settle latency
// (time until replicas == desired), and SLA compliance.
//
// The package never calls the Kubernetes API itself — callers pass
// pre-resolved snapshots so the package stays unit-testable without
// envtest. The autoscaling/v2 condition types come from upstream k8s.io/api
// purely for structural typing.
package hpa

import (
	"fmt"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
)

// Snapshot is the controller's view of an HPA at a single point in time.
type Snapshot struct {
	At              time.Time
	CurrentReplicas int32
	DesiredReplicas int32
	MinReplicas     int32
	MaxReplicas     int32
	Conditions      []autoscalingv2.HorizontalPodAutoscalerCondition
}

// Watcher accumulates snapshots and computes scaling latency on demand.
// It is not safe for concurrent use — the controller holds it from a
// single reconcile goroutine.
type Watcher struct {
	sla      time.Duration
	initial  Snapshot
	samples  []Snapshot
	maxRepl  int32
	settleAt *time.Time
	reactAt  *time.Time
}

// New constructs a Watcher seeded with the initial pre-stress snapshot and
// the configured SLA (e.g. 90s). The SLA bounds reaction + settle combined.
func New(initial Snapshot, sla time.Duration) *Watcher {
	return &Watcher{
		sla:     sla,
		initial: initial,
		samples: []Snapshot{initial},
		maxRepl: initial.CurrentReplicas,
	}
}

// Record appends a snapshot. Triggers internal state transitions:
//   - reactAt is set the first time CurrentReplicas exceeds the initial value.
//   - settleAt is set the first time CurrentReplicas == DesiredReplicas
//     after a scale-up event has been observed.
func (w *Watcher) Record(s Snapshot) {
	w.samples = append(w.samples, s)
	if s.CurrentReplicas > w.maxRepl {
		w.maxRepl = s.CurrentReplicas
	}
	if w.reactAt == nil && s.CurrentReplicas > w.initial.CurrentReplicas {
		at := s.At
		w.reactAt = &at
	}
	if w.reactAt != nil && w.settleAt == nil &&
		s.DesiredReplicas > w.initial.CurrentReplicas &&
		s.CurrentReplicas >= s.DesiredReplicas {
		at := s.At
		w.settleAt = &at
	}
}

// Report is the summary the controller uses to populate ScaleValidationStatus.
type Report struct {
	Started         time.Time
	Ended           time.Time
	StartReplicas   int32
	PeakReplicas    int32
	DesiredReplicas int32
	// ReactionLatency is the gap between Started and the first replica increase.
	// Zero when no scale-up occurred during the window.
	ReactionLatency time.Duration
	// SettleLatency is the gap between Started and the point where
	// CurrentReplicas first reached DesiredReplicas after a scale event.
	// Zero when the deployment never converged within the window.
	SettleLatency time.Duration
	SLA           time.Duration
	SLABreached   bool
	// Reacted indicates whether any scale-up occurred at all during the window.
	Reacted bool
	// Settled indicates whether the deployment converged to desired during the window.
	Settled bool
	// LatestConditions is the conditions slice from the final snapshot.
	LatestConditions []autoscalingv2.HorizontalPodAutoscalerCondition
}

// Report computes the summary across all recorded snapshots. Safe to call
// multiple times.
func (w *Watcher) Report() Report {
	if len(w.samples) == 0 {
		return Report{SLA: w.sla}
	}
	last := w.samples[len(w.samples)-1]
	r := Report{
		Started:          w.initial.At,
		Ended:            last.At,
		StartReplicas:    w.initial.CurrentReplicas,
		PeakReplicas:     w.maxRepl,
		DesiredReplicas:  last.DesiredReplicas,
		SLA:              w.sla,
		LatestConditions: last.Conditions,
	}
	if w.reactAt != nil {
		r.ReactionLatency = w.reactAt.Sub(w.initial.At)
		r.Reacted = true
	}
	if w.settleAt != nil {
		r.SettleLatency = w.settleAt.Sub(w.initial.At)
		r.Settled = true
	}
	// SLA breach when either (a) we never settled within the SLA window,
	// or (b) the settle time itself exceeded the SLA.
	switch {
	case w.sla <= 0:
		// No SLA configured; never flag a breach.
	case !r.Settled:
		// Did we have enough wall time to call a breach? Yes if the window
		// itself exceeded the SLA, otherwise the run was just too short.
		if r.Ended.Sub(r.Started) >= w.sla {
			r.SLABreached = true
		}
	case r.SettleLatency > w.sla:
		r.SLABreached = true
	}
	return r
}

// Diagnostics converts the report into 0..N DiagnosticAlerts.
//   - SLA breach → Critical "HPAScaleLatency"
//   - HPA did not react to load → Warning "HPANoReaction"
//   - HPA reported ScalingLimited=True in the latest snapshot → Warning "HPAScalingLimited"
//   - HPA reported AbleToScale=False in the latest snapshot → Critical "HPAUnableToScale"
func (r Report) Diagnostics() []v1alpha1.DiagnosticAlert {
	var alerts []v1alpha1.DiagnosticAlert
	if r.SLABreached {
		alerts = append(alerts, v1alpha1.DiagnosticAlert{
			Type:     "HPAScaleLatency",
			Severity: "Critical",
			Message: fmt.Sprintf(
				"HPA did not converge within SLA: settle=%s, reaction=%s, SLA=%s, replicas %d → %d (desired %d)",
				r.SettleLatency, r.ReactionLatency, r.SLA,
				r.StartReplicas, r.PeakReplicas, r.DesiredReplicas,
			),
			Recommendation: "tune HPA stabilization window, behavior policies, or metrics provider scrape interval",
		})
	}
	if !r.Reacted && r.DesiredReplicas > r.StartReplicas {
		alerts = append(alerts, v1alpha1.DiagnosticAlert{
			Type:     "HPANoReaction",
			Severity: "Warning",
			Message: fmt.Sprintf(
				"HPA observed desired=%d but currentReplicas never increased from %d during the window",
				r.DesiredReplicas, r.StartReplicas,
			),
			Recommendation: "verify metrics server / external metrics adapter is reporting and HPA targetUtilization is reachable",
		})
	}
	for _, c := range r.LatestConditions {
		switch {
		case c.Type == autoscalingv2.ScalingLimited && c.Status == "True":
			alerts = append(alerts, v1alpha1.DiagnosticAlert{
				Type:           "HPAScalingLimited",
				Severity:       "Warning",
				Message:        fmt.Sprintf("HPA ScalingLimited=True: %s", c.Message),
				Recommendation: "raise the HPA maxReplicas, or address the upstream cap reported by HPA",
			})
		case c.Type == autoscalingv2.AbleToScale && c.Status == "False":
			alerts = append(alerts, v1alpha1.DiagnosticAlert{
				Type:           "HPAUnableToScale",
				Severity:       "Critical",
				Message:        fmt.Sprintf("HPA AbleToScale=False: %s", c.Message),
				Recommendation: "check HPA target ref kind/name, scale subresource availability, and RBAC for the HPA controller",
			})
		}
	}
	return alerts
}
