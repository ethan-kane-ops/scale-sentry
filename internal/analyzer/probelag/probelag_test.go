package probelag

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func t0() time.Time { return time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC) }

func cond(typ corev1.PodConditionType, at time.Time, status corev1.ConditionStatus) corev1.PodCondition {
	return corev1.PodCondition{
		Type:               typ,
		Status:             status,
		LastTransitionTime: metav1.NewTime(at),
	}
}

func TestFromPodConditions_ComputesAllLatencies(t *testing.T) {
	start := t0()
	conds := []corev1.PodCondition{
		cond(corev1.PodScheduled, start, corev1.ConditionTrue),
		cond(corev1.PodInitialized, start.Add(1*time.Second), corev1.ConditionTrue),
		cond(corev1.ContainersReady, start.Add(5*time.Second), corev1.ConditionTrue),
		cond(corev1.PodReady, start.Add(7*time.Second), corev1.ConditionTrue),
	}
	r := FromPodConditions(conds, 5)

	if r.SchedulingLatency != 1*time.Second {
		t.Errorf("SchedulingLatency = %v, want 1s", r.SchedulingLatency)
	}
	if r.StartupLatency != 4*time.Second {
		t.Errorf("StartupLatency = %v, want 4s", r.StartupLatency)
	}
	if r.ReadinessLag != 2*time.Second {
		t.Errorf("ReadinessLag = %v, want 2s", r.ReadinessLag)
	}
	if r.TotalStartupLatency != 7*time.Second {
		t.Errorf("TotalStartupLatency = %v, want 7s", r.TotalStartupLatency)
	}
}

func TestFromPodConditions_IgnoresFalseStatus(t *testing.T) {
	start := t0()
	conds := []corev1.PodCondition{
		cond(corev1.PodScheduled, start, corev1.ConditionTrue),
		cond(corev1.PodReady, start.Add(5*time.Second), corev1.ConditionFalse),
	}
	r := FromPodConditions(conds, 0)
	if r.Ready != nil {
		t.Errorf("Ready = %v, want nil (status was False)", r.Ready)
	}
}

func TestFromPodConditions_NegativeClampedToZero(t *testing.T) {
	start := t0()
	conds := []corev1.PodCondition{
		cond(corev1.ContainersReady, start.Add(10*time.Second), corev1.ConditionTrue),
		cond(corev1.PodReady, start.Add(5*time.Second), corev1.ConditionTrue), // before ContainersReady (impossible but defensive)
	}
	r := FromPodConditions(conds, 0)
	if r.ReadinessLag != 0 {
		t.Errorf("ReadinessLag = %v, want 0 (clock skew defensive)", r.ReadinessLag)
	}
}

func TestDiagnostics_SparseSamplingBands(t *testing.T) {
	start := t0()
	build := func(lag time.Duration) Report {
		return FromPodConditions([]corev1.PodCondition{
			cond(corev1.ContainersReady, start, corev1.ConditionTrue),
			cond(corev1.PodReady, start.Add(lag), corev1.ConditionTrue),
		}, 5)
	}

	tests := []struct {
		name         string
		lag          time.Duration
		wantSeverity string
		wantAlerts   int
	}{
		{"lag below warn threshold (5s with 5s period)", 5 * time.Second, "", 0},
		{"lag below warn (just under 2x period)", 9 * time.Second, "", 0},
		{"lag at warn threshold (2x period)", 10 * time.Second, "Warning", 1},
		{"lag at warn band (3x period)", 15 * time.Second, "Warning", 1},
		{"lag at critical threshold (5x period)", 25 * time.Second, "Critical", 1},
		{"lag well over critical (10x period)", 50 * time.Second, "Critical", 1},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			alerts := build(tc.lag).Diagnostics()
			if len(alerts) != tc.wantAlerts {
				t.Fatalf("got %d alerts, want %d (lag=%v)", len(alerts), tc.wantAlerts, tc.lag)
			}
			if tc.wantAlerts == 0 {
				return
			}
			if alerts[0].Severity != tc.wantSeverity {
				t.Errorf("Severity = %q, want %q", alerts[0].Severity, tc.wantSeverity)
			}
			if alerts[0].Type != "ReadinessSamplingSparse" {
				t.Errorf("Type = %q, want ReadinessSamplingSparse", alerts[0].Type)
			}
		})
	}
}

func TestDiagnostics_ZeroPeriodDisablesCheck(t *testing.T) {
	start := t0()
	r := FromPodConditions([]corev1.PodCondition{
		cond(corev1.ContainersReady, start, corev1.ConditionTrue),
		cond(corev1.PodReady, start.Add(60*time.Second), corev1.ConditionTrue),
	}, 0)
	if alerts := r.Diagnostics(); len(alerts) != 0 {
		t.Errorf("got %d alerts with periodSeconds=0, want 0 (%+v)", len(alerts), alerts)
	}
}

func TestDiagnostics_MissingContainersReady(t *testing.T) {
	start := t0()
	r := FromPodConditions([]corev1.PodCondition{
		cond(corev1.PodReady, start, corev1.ConditionTrue),
	}, 5)
	if alerts := r.Diagnostics(); len(alerts) != 0 {
		t.Errorf("got %d alerts when ContainersReady absent, want 0", len(alerts))
	}
}
