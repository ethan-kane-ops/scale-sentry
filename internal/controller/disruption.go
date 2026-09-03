package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1beta1 "github.com/ethan-kane-ops/scale-sentry/api/v1beta1"
	"github.com/ethan-kane-ops/scale-sentry/internal/chaos"
)

// ConditionDisruptionInjected records the once-per-run disruption decision
// on status.conditions. Status True (reason PodDeleted) means a victim was
// terminated; False (reason Skipped) means the safety gate refused. Its
// presence, either way, is the re-injection guard: a run never disrupts
// twice, even across reconciles.
const ConditionDisruptionInjected = "DisruptionInjected"

// Condition reasons for ConditionDisruptionInjected.
const (
	DisruptionReasonPodDeleted = "PodDeleted"
	DisruptionReasonSkipped    = "Skipped"
)

// maybeInjectDisruption drives spec.disruption while the loadgen Job is in
// flight. Before the trigger point it requeues for the remaining delay; at
// or past it, it evaluates chaos.Plan once against the target's current
// pods and either deletes the victim or records the skip. The decision is
// deliberately single-shot: retrying a skipped injection until enough
// replicas appear would turn the safety gate into a race.
func (r *ScaleValidationReconciler) maybeInjectDisruption(ctx context.Context, cr *v1beta1.ScaleValidation) (ctrl.Result, error) {
	d := cr.Spec.Disruption
	if d == nil || !d.InjectPodDeletion || cr.Status.Phase != PhaseRunning {
		return ctrl.Result{}, nil
	}
	if meta.FindStatusCondition(cr.Status.Conditions, ConditionDisruptionInjected) != nil {
		return ctrl.Result{}, nil
	}

	// LastRunTime is written by the same status update that moves the CR
	// to Running, but a hand-crafted CR could carry the phase without it.
	loadStart := cr.CreationTimestamp.Time
	if cr.Status.LastRunTime != nil {
		loadStart = cr.Status.LastRunTime.Time
	}
	var delay time.Duration
	if d.TriggerDelay != nil {
		delay = d.TriggerDelay.Duration
	}
	if wait := time.Until(loadStart.Add(delay)); wait > 0 {
		return ctrl.Result{RequeueAfter: wait}, nil
	}

	pods, err := r.targetPods(ctx, cr)
	if err != nil {
		return ctrl.Result{}, err
	}
	decision := chaos.Plan(chaos.Config{
		InjectPodDeletion:   d.InjectPodDeletion,
		MinReplicasForChaos: d.MinReplicasForChaos,
		TriggerDelay:        delay,
	}, loadStart, pods, nil)

	if !decision.Inject {
		logf.FromContext(ctx).Info("disruption skipped", "reason", decision.SkipReason)
		r.eventf(cr, corev1.EventTypeWarning, EventReasonChaosSkipped, "%s", decision.SkipReason)
		return r.recordDisruption(ctx, cr, metav1.ConditionFalse, DisruptionReasonSkipped, decision.SkipReason)
	}

	// A victim vanishing between List and Delete is already the endpoint
	// churn the run wants to observe, so NotFound counts as injected.
	if err := r.Delete(ctx, decision.Victim); err != nil && !apierrors.IsNotFound(err) {
		return ctrl.Result{}, fmt.Errorf("delete disruption victim %s: %w", decision.Victim.Name, err)
	}
	msg := fmt.Sprintf("deleted pod %s during load (%d healthy replicas)",
		decision.Victim.Name, decision.HealthyCount)
	logf.FromContext(ctx).Info("disruption injected",
		"victim", decision.Victim.Name, "healthy", decision.HealthyCount)
	r.eventf(cr, corev1.EventTypeNormal, EventReasonChaosInjected, "%s", msg)
	return r.recordDisruption(ctx, cr, metav1.ConditionTrue, DisruptionReasonPodDeleted, msg)
}

// targetPods lists the pods backing the CR's target Deployment via the
// Deployment's own label selector, so victim selection sees exactly the
// replica set the HPA manages (never the loadgen Job pod).
func (r *ScaleValidationReconciler) targetPods(ctx context.Context, cr *v1beta1.ScaleValidation) ([]corev1.Pod, error) {
	var deploy appsv1.Deployment
	key := types.NamespacedName{Namespace: cr.Namespace, Name: cr.Spec.TargetRef.Name}
	if err := r.Get(ctx, key, &deploy); err != nil {
		return nil, fmt.Errorf("get target deployment %s: %w", cr.Spec.TargetRef.Name, err)
	}
	sel, err := metav1.LabelSelectorAsSelector(deploy.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("target deployment selector: %w", err)
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(cr.Namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return nil, fmt.Errorf("list target pods: %w", err)
	}
	return pods.Items, nil
}

// recordDisruption persists the disruption decision as a status condition.
func (r *ScaleValidationReconciler) recordDisruption(ctx context.Context, cr *v1beta1.ScaleValidation, status metav1.ConditionStatus, reason, msg string) (ctrl.Result, error) {
	meta.SetStatusCondition(&cr.Status.Conditions, metav1.Condition{
		Type:               ConditionDisruptionInjected,
		Status:             status,
		Reason:             reason,
		Message:            msg,
		ObservedGeneration: cr.Generation,
	})
	if err := r.Status().Update(ctx, cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("record disruption decision: %w", err)
	}
	return ctrl.Result{}, nil
}
