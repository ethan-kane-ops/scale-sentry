package controller

import (
	"context"
	"fmt"
	"time"

	"github.com/robfig/cron/v3"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1beta1 "github.com/ethan-kane-ops/scale-sentry/api/v1beta1"
	"github.com/ethan-kane-ops/scale-sentry/internal/metrics"
)

// scheduleParser accepts standard five-field cron plus the descriptors
// (@hourly, @daily, @every 1h30m), matching what a CronJob accepts so the
// syntax is already familiar.
var scheduleParser = cron.NewParser(
	cron.Minute | cron.Hour | cron.Dom | cron.Month | cron.Dow | cron.Descriptor,
)

// parseSchedule compiles spec.schedule. An empty schedule is not an error:
// it means the validation runs exactly once, which is the default.
func parseSchedule(cr *v1beta1.ScaleValidation) (cron.Schedule, error) {
	if cr.Spec.Schedule == "" {
		return nil, nil
	}
	sched, err := scheduleParser.Parse(cr.Spec.Schedule)
	if err != nil {
		return nil, fmt.Errorf("parse schedule %q: %w", cr.Spec.Schedule, err)
	}
	return sched, nil
}

// lastRunAnchor is the point the next run is measured from: when the last
// run started, matching CronJob, so a run that overruns does not push the
// whole schedule out. Falls back to creation for a CR that somehow reached
// a terminal phase without ever spawning a Job.
func lastRunAnchor(cr *v1beta1.ScaleValidation) time.Time {
	if cr.Status.LastRunTime != nil {
		return cr.Status.LastRunTime.Time
	}
	return cr.CreationTimestamp.Time
}

// reconcileTerminal decides what happens to a CR that has reached a
// terminal phase. Without spec.schedule that is nothing at all, which is
// the one-shot behaviour every existing CR relies on. With one, it either
// parks until the next due time or starts the next run.
func (r *ScaleValidationReconciler) reconcileTerminal(ctx context.Context, cr *v1beta1.ScaleValidation) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if cr.Status.ObservedGeneration == 0 {
		// A CR that finished before observedGeneration existed. Adopt the
		// current generation rather than reading the absence as drift,
		// which would re-run every historical CR once on upgrade.
		cr.Status.ObservedGeneration = cr.Generation
		if err := r.Status().Update(ctx, cr); err != nil {
			return ctrl.Result{}, fmt.Errorf("adopt observed generation: %w", err)
		}
		return ctrl.Result{}, nil
	}

	// Suspend outranks everything below, including a spec edit. Setting
	// suspend is itself an edit, so checking drift first would make the
	// act of suspending start a run, which inverts the field.
	if cr.Spec.Suspend {
		return r.parkSuspended(ctx, cr)
	}

	if cr.Generation != cr.Status.ObservedGeneration {
		return r.restartForSpecChange(ctx, cr)
	}

	sched, err := parseSchedule(cr)
	if err != nil {
		// The schedule is validated before the first run, so reaching
		// here means the spec was edited to something invalid after the
		// fact. Stop rescheduling rather than hot-looping on a parse.
		log.Error(err, "invalid schedule on a terminal CR, not rescheduling")
		return ctrl.Result{}, nil
	}
	if sched == nil {
		return ctrl.Result{}, nil
	}

	next := sched.Next(lastRunAnchor(cr))
	if remaining := next.Sub(r.now()); remaining > 0 {
		if cr.Status.NextRunTime == nil || !cr.Status.NextRunTime.Time.Equal(next) {
			nextTime := metav1.NewTime(next)
			cr.Status.NextRunTime = &nextTime
			if err := r.Status().Update(ctx, cr); err != nil {
				return ctrl.Result{}, fmt.Errorf("publish next run time: %w", err)
			}
		}
		return ctrl.Result{RequeueAfter: remaining}, nil
	}
	return r.startNextRun(ctx, cr)
}

// startNextRun tears down the finished loadgen Job, resets the run-scoped
// status, and puts the CR back to Pending so the normal spawn path picks
// it up. The Job name is derived from the CR name, so the old one has to
// be gone before a new one can be created.
func (r *ScaleValidationReconciler) startNextRun(ctx context.Context, cr *v1beta1.ScaleValidation) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var job batchv1.Job
	key := types.NamespacedName{Namespace: cr.Namespace, Name: loadgenJobName(cr)}
	switch err := r.Get(ctx, key, &job); {
	case err == nil:
		if job.DeletionTimestamp == nil {
			policy := metav1.DeletePropagationBackground
			if err := r.Delete(ctx, &job, &client.DeleteOptions{PropagationPolicy: &policy}); err != nil &&
				!apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("delete finished loadgen job: %w", err)
			}
		}
		// Come back once the apiserver has cleared it.
		return ctrl.Result{RequeueAfter: childTerminationRequeue}, nil
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, fmt.Errorf("get loadgen job: %w", err)
	}

	resetForNextRun(cr)
	log.Info("starting next scheduled run", "schedule", cr.Spec.Schedule)
	r.eventf(cr, corev1.EventTypeNormal, EventReasonRunScheduled,
		"starting the next run on schedule %q", cr.Spec.Schedule)
	return r.setPhase(ctx, cr, PhasePending)
}

// resetForNextRun clears everything scoped to the run that just ended, so
// the next one reports its own results rather than inheriting the last
// one's. status.history is deliberately kept, it is the whole point of
// running on a schedule.
func resetForNextRun(cr *v1beta1.ScaleValidation) {
	cr.Status.Diagnostics = nil
	cr.Status.ScaleUpDuration = nil
	cr.Status.SLAStatus = ""
	cr.Status.TrafficIntegrity = ""
	cr.Status.TotalRequests = 0
	cr.Status.FailedRequests = 0
	cr.Status.FailureRateBasisPoints = 0
	cr.Status.NextRunTime = nil

	// The presence of DisruptionInjected is the once-per-run chaos guard.
	// Leaving it set would silently disable chaos on every run after the
	// first.
	meta.RemoveStatusCondition(&cr.Status.Conditions, ConditionDisruptionInjected)

	// Finished goes back to False so a watcher blocking on it waits for
	// this run rather than returning on the previous run's verdict.
	meta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               ConditionFinished,
		Status:             metav1.ConditionFalse,
		ObservedGeneration: cr.Generation,
		Reason:             FinishedReasonRescheduled,
		Message:            fmt.Sprintf("a new run started on schedule %q", cr.Spec.Schedule),
	})
}

// failScheduleInvalid rejects an unparseable spec.schedule up front, on
// the first reconcile, rather than letting the CR run once and then stall
// silently at its first scheduling decision.
func (r *ScaleValidationReconciler) failScheduleInvalid(ctx context.Context, cr *v1beta1.ScaleValidation, cause error) (ctrl.Result, error) {
	msg := fmt.Sprintf("spec.schedule %q is not a valid cron expression: %v", cr.Spec.Schedule, cause)
	cr.Status.Diagnostics = append(cr.Status.Diagnostics, v1beta1.DiagnosticAlert{
		Type:           "ScheduleInvalid",
		Severity:       "Critical",
		Message:        msg,
		Recommendation: "use standard five-field cron (\"0 2 * * *\") or a descriptor (@daily, @every 30m)",
	})
	r.eventf(cr, corev1.EventTypeWarning, EventReasonScheduleInvalid, "%s", msg)
	metrics.RunsTotal.WithLabelValues(cr.Namespace, cr.Name, metrics.VerdictUnknown).Inc()
	metrics.DiagnosticAlertsTotal.WithLabelValues(cr.Namespace, cr.Name, "ScheduleInvalid", "Critical").Inc()
	return r.setTerminalPhase(ctx, cr, PhaseError, FinishedReasonScheduleInvalid, msg)
}

// parkSuspended holds a suspended CR still. It drops any advertised next
// run so `kubectl get` does not promise something suspend will never
// deliver, and records the generation so the edit that suspended the CR is
// not later replayed as drift. Writes only when something actually
// changed, so a suspended CR does not churn its status on every reconcile.
func (r *ScaleValidationReconciler) parkSuspended(ctx context.Context, cr *v1beta1.ScaleValidation) (ctrl.Result, error) {
	changed := false
	if cr.Status.NextRunTime != nil {
		cr.Status.NextRunTime = nil
		changed = true
	}
	if cr.Status.ObservedGeneration != cr.Generation {
		cr.Status.ObservedGeneration = cr.Generation
		changed = true
	}
	if !changed {
		return ctrl.Result{}, nil
	}
	if err := r.Status().Update(ctx, cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("park suspended validation: %w", err)
	}
	return ctrl.Result{}, nil
}

// restartForSpecChange handles a terminal CR whose spec has been edited
// since the result on it was produced. The result describes a spec the
// object no longer carries, so it is replaced by a fresh run rather than
// left to mislead.
//
// The schedule is revalidated first. Without that, a CR parked in Error on
// an unparseable schedule would run once more before failing again, and
// the failure message would describe the run rather than the schedule.
// Validating here is also what lets a broken schedule be fixed in place:
// the edit bumps the generation, the new value parses, and the CR
// recovers without being deleted and recreated.
func (r *ScaleValidationReconciler) restartForSpecChange(ctx context.Context, cr *v1beta1.ScaleValidation) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if _, err := parseSchedule(cr); err != nil {
		// The previous run's findings describe the old spec.
		cr.Status.Diagnostics = nil
		return r.failScheduleInvalid(ctx, cr, err)
	}

	log.Info("spec changed since the last result, starting a new run",
		"generation", cr.Generation, "observedGeneration", cr.Status.ObservedGeneration)
	r.eventf(cr, corev1.EventTypeNormal, EventReasonSpecChanged,
		"spec changed (generation %d supersedes %d), starting a new run",
		cr.Generation, cr.Status.ObservedGeneration)
	return r.startNextRun(ctx, cr)
}
