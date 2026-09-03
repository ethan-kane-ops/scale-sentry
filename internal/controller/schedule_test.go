package controller

import (
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
)

func TestParseSchedule(t *testing.T) {
	tests := []struct {
		name     string
		schedule string
		wantNil  bool
		wantErr  bool
	}{
		{"empty means one-shot", "", true, false},
		{"five-field cron", "0 2 * * *", false, false},
		{"every-weekday cron", "30 6 * * 1-5", false, false},
		{"daily descriptor", "@daily", false, false},
		{"every descriptor", "@every 30m", false, false},
		{"seconds field is rejected", "0 0 2 * * *", true, true},
		{"gibberish", "not a schedule", true, true},
		{"out of range", "99 * * * *", true, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cr := &v1alpha1.ScaleValidation{
				Spec: v1alpha1.ScaleValidationSpec{Schedule: tt.schedule},
			}
			sched, err := parseSchedule(cr)
			if tt.wantErr && err == nil {
				t.Fatalf("parseSchedule(%q) = nil error, want one", tt.schedule)
			}
			if !tt.wantErr && err != nil {
				t.Fatalf("parseSchedule(%q): %v", tt.schedule, err)
			}
			if (sched == nil) != tt.wantNil {
				t.Errorf("parseSchedule(%q) nil = %v, want %v", tt.schedule, sched == nil, tt.wantNil)
			}
		})
	}
}

func TestLastRunAnchor(t *testing.T) {
	created := time.Date(2026, 9, 1, 10, 0, 0, 0, time.UTC)
	ran := time.Date(2026, 9, 3, 4, 30, 0, 0, time.UTC)

	cr := &v1alpha1.ScaleValidation{
		ObjectMeta: metav1.ObjectMeta{CreationTimestamp: metav1.NewTime(created)},
	}
	if got := lastRunAnchor(cr); !got.Equal(created) {
		t.Errorf("anchor without lastRunTime = %v, want creation %v", got, created)
	}

	lrt := metav1.NewTime(ran)
	cr.Status.LastRunTime = &lrt
	if got := lastRunAnchor(cr); !got.Equal(ran) {
		t.Errorf("anchor = %v, want lastRunTime %v", got, ran)
	}
}

// TestResetForNextRun pins what a re-run must forget and what it must
// keep. The two conditions matter most: DisruptionInjected is the
// once-per-run chaos guard, so leaving it set would silently disable chaos
// on every run after the first, and Finished must drop to False or a
// watcher would return on the previous run's verdict.
func TestResetForNextRun(t *testing.T) {
	dur := metav1.Duration{Duration: 42 * time.Second}
	cr := &v1alpha1.ScaleValidation{
		Spec: v1alpha1.ScaleValidationSpec{Schedule: "@daily"},
		Status: v1alpha1.ScaleValidationStatus{
			Diagnostics:      []v1alpha1.DiagnosticAlert{{Type: "CPUThrottling", Severity: "Warning", Message: "old"}},
			ScaleUpDuration:  &dur,
			SLAStatus:        "Fail",
			TrafficIntegrity: "Pass",
			TotalRequests:    5000,
			FailedRequests:   12,
			FailureRate:      0.0024,
			History:          []v1alpha1.RunSummary{{Phase: PhaseFailed}, {Phase: PhaseSucceeded}},
		},
	}
	markFinished(cr, FinishedReasonVerdictFailed, "previous run")
	meta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type: ConditionDisruptionInjected, Status: metav1.ConditionTrue,
		Reason: DisruptionReasonPodDeleted, Message: "deleted a victim",
	})
	next := metav1.NewTime(time.Now())
	cr.Status.NextRunTime = &next

	resetForNextRun(cr)

	if cr.Status.Diagnostics != nil {
		t.Errorf("diagnostics = %+v, want nil (a new run reports its own)", cr.Status.Diagnostics)
	}
	if cr.Status.ScaleUpDuration != nil || cr.Status.SLAStatus != "" || cr.Status.TrafficIntegrity != "" {
		t.Errorf("verdict fields not cleared: %+v", cr.Status)
	}
	if cr.Status.TotalRequests != 0 || cr.Status.FailedRequests != 0 || cr.Status.FailureRate != 0 {
		t.Errorf("request counters not cleared: %+v", cr.Status)
	}
	if cr.Status.NextRunTime != nil {
		t.Errorf("nextRunTime = %v, want nil once the run has started", cr.Status.NextRunTime)
	}
	if len(cr.Status.History) != 2 {
		t.Errorf("history = %d entries, want 2 kept (history is the point of scheduling)", len(cr.Status.History))
	}
	if cond := meta.FindStatusCondition(cr.Status.Conditions, ConditionDisruptionInjected); cond != nil {
		t.Errorf("%s still set: %+v, chaos would never fire again", ConditionDisruptionInjected, cond)
	}
	cond := meta.FindStatusCondition(cr.Status.Conditions, ConditionFinished)
	if cond == nil || cond.Status != metav1.ConditionFalse {
		t.Fatalf("%s = %+v, want False for a run that has restarted", ConditionFinished, cond)
	}
	if cond.Reason != FinishedReasonRescheduled {
		t.Errorf("reason = %s, want %s", cond.Reason, FinishedReasonRescheduled)
	}
}
