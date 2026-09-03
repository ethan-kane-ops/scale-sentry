package controller

import (
	"strings"
	"testing"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
	"github.com/ethan-kane-ops/scale-sentry/internal/observer"
)

func TestFinishedVerdict(t *testing.T) {
	tests := []struct {
		name       string
		sla        string
		traffic    string
		wantPhase  string
		wantReason string
	}{
		{"both pass", observer.VerdictPass, observer.VerdictPass, PhaseSucceeded, FinishedReasonSucceeded},
		{"sla fail", observer.VerdictFail, observer.VerdictPass, PhaseFailed, FinishedReasonVerdictFailed},
		{"traffic fail", observer.VerdictPass, observer.VerdictFail, PhaseFailed, FinishedReasonVerdictFailed},
		{"both fail", observer.VerdictFail, observer.VerdictFail, PhaseFailed, FinishedReasonVerdictFailed},
		// Unknown is not a failure: a run that could not measure still
		// reports its phase from the surrounding error path, not here.
		{"unknown passes through", observer.VerdictUnknown, observer.VerdictUnknown, PhaseSucceeded, FinishedReasonSucceeded},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			phase, reason, message := finishedVerdict(tt.sla, tt.traffic, 5000, 3)
			if phase != tt.wantPhase || reason != tt.wantReason {
				t.Errorf("finishedVerdict = (%s, %s), want (%s, %s)", phase, reason, tt.wantPhase, tt.wantReason)
			}
			if !strings.Contains(message, "requests=5000") || !strings.Contains(message, "failed=3") {
				t.Errorf("message should carry the request counts, got %q", message)
			}
		})
	}
}

func TestMarkFinished(t *testing.T) {
	cr := &v1alpha1.ScaleValidation{}
	cr.Generation = 4

	markFinished(cr, FinishedReasonSucceeded, "SLA=Pass traffic=Pass")
	cond := meta.FindStatusCondition(cr.Status.Conditions, ConditionFinished)
	if cond == nil {
		t.Fatal("Finished condition not set")
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("status = %s, want True (Finished means terminal, not passed)", cond.Status)
	}
	if cond.Reason != FinishedReasonSucceeded {
		t.Errorf("reason = %s, want %s", cond.Reason, FinishedReasonSucceeded)
	}
	if cond.ObservedGeneration != 4 {
		t.Errorf("observedGeneration = %d, want 4", cond.ObservedGeneration)
	}
}

// TestMarkFinished_TransitionTimeStableOnRepeat guards the property
// meta.SetStatusCondition provides: re-marking with the same status must
// not churn lastTransitionTime, or every requeue would look like a fresh
// terminal event to anything watching.
func TestMarkFinished_TransitionTimeStableOnRepeat(t *testing.T) {
	cr := &v1alpha1.ScaleValidation{}
	markFinished(cr, FinishedReasonSucceeded, "first")
	first := meta.FindStatusCondition(cr.Status.Conditions, ConditionFinished).LastTransitionTime

	markFinished(cr, FinishedReasonVerdictFailed, "second")
	cond := meta.FindStatusCondition(cr.Status.Conditions, ConditionFinished)
	if !cond.LastTransitionTime.Equal(&first) {
		t.Errorf("lastTransitionTime moved on a same-status update: %v then %v", first, cond.LastTransitionTime)
	}
	if cond.Reason != FinishedReasonVerdictFailed || cond.Message != "second" {
		t.Errorf("reason/message did not update: %s / %s", cond.Reason, cond.Message)
	}
	if len(cr.Status.Conditions) != 1 {
		t.Errorf("conditions = %d, want 1 (Finished must not be duplicated)", len(cr.Status.Conditions))
	}
}
