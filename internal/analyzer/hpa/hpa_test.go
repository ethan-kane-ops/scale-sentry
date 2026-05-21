package hpa

import (
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func t0() time.Time { return time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC) }

func TestWatcher_ReactionAndSettleLatency(t *testing.T) {
	start := t0()
	w := New(Snapshot{At: start, CurrentReplicas: 2, DesiredReplicas: 2}, 90*time.Second)
	w.Record(Snapshot{At: start.Add(5 * time.Second), CurrentReplicas: 2, DesiredReplicas: 5})
	w.Record(Snapshot{At: start.Add(10 * time.Second), CurrentReplicas: 3, DesiredReplicas: 5})
	w.Record(Snapshot{At: start.Add(20 * time.Second), CurrentReplicas: 5, DesiredReplicas: 5})

	r := w.Report()
	if r.ReactionLatency != 10*time.Second {
		t.Errorf("ReactionLatency = %v, want 10s", r.ReactionLatency)
	}
	if r.SettleLatency != 20*time.Second {
		t.Errorf("SettleLatency = %v, want 20s", r.SettleLatency)
	}
	if r.PeakReplicas != 5 {
		t.Errorf("PeakReplicas = %d, want 5", r.PeakReplicas)
	}
	if !r.Reacted {
		t.Error("Reacted = false, want true")
	}
	if !r.Settled {
		t.Error("Settled = false, want true")
	}
	if r.SLABreached {
		t.Error("SLABreached = true, want false (settle 20s < SLA 90s)")
	}
}

func TestWatcher_SLABreach_SettleExceedsSLA(t *testing.T) {
	start := t0()
	w := New(Snapshot{At: start, CurrentReplicas: 2, DesiredReplicas: 2}, 15*time.Second)
	w.Record(Snapshot{At: start.Add(5 * time.Second), CurrentReplicas: 3, DesiredReplicas: 5})
	w.Record(Snapshot{At: start.Add(30 * time.Second), CurrentReplicas: 5, DesiredReplicas: 5})

	r := w.Report()
	if !r.SLABreached {
		t.Error("SLABreached = false, want true (settle 30s > SLA 15s)")
	}
	if !r.Settled {
		t.Error("Settled = false, want true")
	}
}

func TestWatcher_SLABreach_NeverSettled(t *testing.T) {
	start := t0()
	w := New(Snapshot{At: start, CurrentReplicas: 2, DesiredReplicas: 2}, 10*time.Second)
	w.Record(Snapshot{At: start.Add(5 * time.Second), CurrentReplicas: 3, DesiredReplicas: 5})
	w.Record(Snapshot{At: start.Add(20 * time.Second), CurrentReplicas: 4, DesiredReplicas: 5})

	r := w.Report()
	if r.Settled {
		t.Error("Settled = true, want false")
	}
	if !r.SLABreached {
		t.Error("SLABreached = false, want true (window 20s exceeded SLA 10s with no convergence)")
	}
}

func TestWatcher_NoBreach_WindowTooShortToCallIt(t *testing.T) {
	start := t0()
	w := New(Snapshot{At: start, CurrentReplicas: 2, DesiredReplicas: 2}, 90*time.Second)
	w.Record(Snapshot{At: start.Add(5 * time.Second), CurrentReplicas: 3, DesiredReplicas: 5})

	r := w.Report()
	if r.SLABreached {
		t.Error("SLABreached = true, want false (window 5s shorter than SLA 90s)")
	}
}

func TestWatcher_ZeroSLANeverBreaches(t *testing.T) {
	start := t0()
	w := New(Snapshot{At: start, CurrentReplicas: 2, DesiredReplicas: 2}, 0)
	w.Record(Snapshot{At: start.Add(120 * time.Second), CurrentReplicas: 2, DesiredReplicas: 5})

	r := w.Report()
	if r.SLABreached {
		t.Error("SLABreached = true, want false (SLA disabled)")
	}
}

func TestReport_Diagnostics_NoReaction(t *testing.T) {
	start := t0()
	w := New(Snapshot{At: start, CurrentReplicas: 2, DesiredReplicas: 2}, 30*time.Second)
	w.Record(Snapshot{At: start.Add(60 * time.Second), CurrentReplicas: 2, DesiredReplicas: 5})

	alerts := w.Report().Diagnostics()
	var found bool
	for _, a := range alerts {
		if a.Type == "HPANoReaction" {
			found = true
			if a.Severity != "Warning" {
				t.Errorf("HPANoReaction severity = %q, want Warning", a.Severity)
			}
		}
	}
	if !found {
		t.Errorf("expected HPANoReaction alert, got %+v", alerts)
	}
}

func TestReport_Diagnostics_ScalingLimitedAndUnableToScale(t *testing.T) {
	start := t0()
	conds := []autoscalingv2.HorizontalPodAutoscalerCondition{
		{
			Type:               autoscalingv2.ScalingLimited,
			Status:             "True",
			Message:            "the desired replica count is more than the maximum replica count",
			LastTransitionTime: metav1.NewTime(start),
		},
		{
			Type:               autoscalingv2.AbleToScale,
			Status:             "False",
			Message:            "the HPA controller was unable to update the target scale",
			LastTransitionTime: metav1.NewTime(start),
		},
	}
	w := New(Snapshot{At: start, CurrentReplicas: 3, DesiredReplicas: 3}, 60*time.Second)
	w.Record(Snapshot{At: start.Add(10 * time.Second), CurrentReplicas: 3, DesiredReplicas: 10, Conditions: conds})

	alerts := w.Report().Diagnostics()
	seen := map[string]string{}
	for _, a := range alerts {
		seen[a.Type] = a.Severity
	}
	if seen["HPAScalingLimited"] != "Warning" {
		t.Errorf("HPAScalingLimited severity = %q, want Warning (full: %+v)", seen["HPAScalingLimited"], alerts)
	}
	if seen["HPAUnableToScale"] != "Critical" {
		t.Errorf("HPAUnableToScale severity = %q, want Critical (full: %+v)", seen["HPAUnableToScale"], alerts)
	}
}

func TestReport_Diagnostics_BreachIsCritical(t *testing.T) {
	start := t0()
	w := New(Snapshot{At: start, CurrentReplicas: 2, DesiredReplicas: 2}, 5*time.Second)
	w.Record(Snapshot{At: start.Add(2 * time.Second), CurrentReplicas: 3, DesiredReplicas: 5})
	w.Record(Snapshot{At: start.Add(20 * time.Second), CurrentReplicas: 5, DesiredReplicas: 5})

	alerts := w.Report().Diagnostics()
	if len(alerts) == 0 || alerts[0].Type != "HPAScaleLatency" || alerts[0].Severity != "Critical" {
		t.Errorf("expected HPAScaleLatency Critical, got %+v", alerts)
	}
}
