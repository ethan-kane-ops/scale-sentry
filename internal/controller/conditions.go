package controller

import (
	"context"
	"fmt"

	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	ctrl "sigs.k8s.io/controller-runtime"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
	"github.com/ethan-kane-ops/scale-sentry/internal/observer"
)

// ConditionFinished is set True the moment a run reaches a terminal
// phase, whatever the verdict was.
//
// It is deliberately not a pass/fail signal. `kubectl wait --for=condition=X`
// only ever waits for X to become True, so a condition that goes False on
// failure would leave a CI gate blocking until its own --timeout, which is
// exactly the behaviour this replaces. One condition that flips True on
// any terminal outcome, plus a read of status.phase, gives a pipeline the
// verdict within seconds of the run ending:
//
//	kubectl wait scalevalidation/x --for=condition=Finished --timeout=15m
//	kubectl get scalevalidation/x -o jsonpath='{.status.phase}'
//
// Reason carries why the run ended; the phase carries the verdict.
const ConditionFinished = "Finished"

// Reasons for ConditionFinished. Succeeded and VerdictFailed are the two
// outcomes of a run that actually measured something; the rest name a
// specific reason the run never produced a verdict. Rescheduled is the one
// reason paired with Status False: a scheduled validation starting its
// next run. They are stable strings, so alert routers can match on them
// without parsing messages.
const (
	FinishedReasonSucceeded                = "Succeeded"
	FinishedReasonVerdictFailed            = "VerdictFailed"
	FinishedReasonTargetNotReady           = "TargetNotReady"
	FinishedReasonTargetUnsupported        = "TargetUnsupported"
	FinishedReasonTLSCABundleMissing       = "TLSCABundleMissing"
	FinishedReasonLoadgenJobFailed         = "LoadgenJobFailed"
	FinishedReasonLoadgenJobVanished       = "LoadgenJobVanished"
	FinishedReasonObserverReportUnreadable = "ObserverReportUnreadable"
	FinishedReasonTargetURLUnresolved      = "TargetURLUnresolved"
	FinishedReasonJobBuildFailed           = "JobBuildFailed"
	FinishedReasonScheduleInvalid          = "ScheduleInvalid"
	FinishedReasonRescheduled              = "Rescheduled"
)

// markFinished stages the Finished condition on cr without writing it.
// Used by callers that already own a Status().Update and need the
// condition to land in the same write as the rest of the run results.
func markFinished(cr *v1alpha1.ScaleValidation, reason, message string) {
	meta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               ConditionFinished,
		Status:             metav1.ConditionTrue,
		ObservedGeneration: cr.Generation,
		Reason:             reason,
		Message:            message,
	})
}

// setTerminalPhase stages the Finished condition and the terminal phase,
// then writes both in a single status update so a watcher never observes
// a terminal phase without the condition that explains it.
func (r *ScaleValidationReconciler) setTerminalPhase(ctx context.Context, cr *v1alpha1.ScaleValidation, phase, reason, message string) (ctrl.Result, error) {
	markFinished(cr, reason, message)
	return r.setPhase(ctx, cr, phase)
}

// finishedVerdict maps an observer verdict onto the terminal phase and the
// Finished reason + message that describe it.
func finishedVerdict(slaStatus, trafficIntegrity string, totalRequests, failedRequests int64) (phase, reason, message string) {
	summary := fmt.Sprintf("SLA=%s traffic=%s requests=%d failed=%d",
		slaStatus, trafficIntegrity, totalRequests, failedRequests)
	if slaStatus == observer.VerdictFail || trafficIntegrity == observer.VerdictFail {
		return PhaseFailed, FinishedReasonVerdictFailed, summary
	}
	return PhaseSucceeded, FinishedReasonSucceeded, summary
}
