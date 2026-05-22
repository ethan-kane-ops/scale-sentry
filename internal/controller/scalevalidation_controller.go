// Package controller holds the controller-runtime reconcilers that drive
// scale-sentry as a Kubernetes operator. The ScaleValidationReconciler runs
// a validation by spawning a loadgen Job and tracking its lifecycle; the
// DeploymentShadowReconciler auto-creates ScaleValidations for annotated
// Deployments.
//
// The reconcilers are the only place in the codebase that talk to the
// Kubernetes API — the analyzer and loadgen packages stay client-free.
package controller

import (
	"context"
	"fmt"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
)

// Lifecycle phases written to status.phase. Pending and Running are
// transient; Succeeded, Failed, and Error are terminal. Failed (the SLA /
// traffic-integrity verdict) is wired once result parsing lands in ENG-36 —
// ENG-35 only distinguishes Succeeded (the run completed) from Error (the
// run could not be carried out).
const (
	PhasePending   = "Pending"
	PhaseRunning   = "Running"
	PhaseSucceeded = "Succeeded"
	PhaseFailed    = "Failed"
	PhaseError     = "Error"
)

// ScaleValidationReconciler reconciles a ScaleValidation object.
type ScaleValidationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// LoadgenImage is the container image used for the spawned loadgen Job.
	LoadgenImage string
}

//+kubebuilder:rbac:groups=validation.scale-sentry.ek.co,resources=scalevalidations,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups=validation.scale-sentry.ek.co,resources=scalevalidations/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=validation.scale-sentry.ek.co,resources=scalevalidations/finalizers,verbs=update
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
//+kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives the ScaleValidation lifecycle: empty -> Pending -> spawn
// loadgen Job -> Running -> Succeeded/Error once the Job reaches a terminal
// condition. Terminal phases are not re-run.
func (r *ScaleValidationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cr v1alpha1.ScaleValidation
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	switch cr.Status.Phase {
	case PhaseSucceeded, PhaseFailed, PhaseError:
		return ctrl.Result{}, nil
	case "":
		return r.setPhase(ctx, &cr, PhasePending)
	}

	var job batchv1.Job
	jobKey := types.NamespacedName{Namespace: cr.Namespace, Name: loadgenJobName(&cr)}
	err := r.Get(ctx, jobKey, &job)
	switch {
	case apierrors.IsNotFound(err):
		if cr.Status.Phase == PhaseRunning {
			log.Info("loadgen job vanished before completion", "job", jobKey.Name)
			return r.setPhase(ctx, &cr, PhaseError)
		}
		return r.spawnJob(ctx, &cr)
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("get loadgen job: %w", err)
	}

	switch {
	case jobConditionTrue(&job, batchv1.JobComplete):
		log.Info("loadgen job complete", "job", job.Name)
		return r.setPhase(ctx, &cr, PhaseSucceeded)
	case jobConditionTrue(&job, batchv1.JobFailed):
		log.Info("loadgen job failed", "job", job.Name)
		return r.setPhase(ctx, &cr, PhaseError)
	default:
		return ctrl.Result{}, nil
	}
}

// spawnJob creates the loadgen Job for cr and moves it to Running.
func (r *ScaleValidationReconciler) spawnJob(ctx context.Context, cr *v1alpha1.ScaleValidation) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	job := r.buildLoadgenJob(cr)
	if err := controllerutil.SetControllerReference(cr, job, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("set owner reference: %w", err)
	}
	if err := r.Create(ctx, job); err != nil {
		return ctrl.Result{}, fmt.Errorf("create loadgen job: %w", err)
	}
	log.Info("spawned loadgen job", "job", job.Name)

	now := metav1.Now()
	cr.Status.LastRunTime = &now
	return r.setPhase(ctx, cr, PhaseRunning)
}

// setPhase persists phase to status.phase via the status subresource.
func (r *ScaleValidationReconciler) setPhase(ctx context.Context, cr *v1alpha1.ScaleValidation, phase string) (ctrl.Result, error) {
	cr.Status.Phase = phase
	if err := r.Status().Update(ctx, cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("update status to %s: %w", phase, err)
	}
	return ctrl.Result{}, nil
}

// jobConditionTrue reports whether job carries condition t with status True.
func jobConditionTrue(job *batchv1.Job, t batchv1.JobConditionType) bool {
	for _, c := range job.Status.Conditions {
		if c.Type == t && c.Status == corev1.ConditionTrue {
			return true
		}
	}
	return false
}

// SetupWithManager registers the reconciler. Owns(&batchv1.Job{}) means a
// Job condition transition re-triggers reconciliation of its owner CR.
func (r *ScaleValidationReconciler) SetupWithManager(mgr ctrl.Manager) error {
	return ctrl.NewControllerManagedBy(mgr).
		For(&v1alpha1.ScaleValidation{}).
		Owns(&batchv1.Job{}).
		Named("scalevalidation").
		Complete(r)
}
