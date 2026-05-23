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
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
	"github.com/ethan-kane-ops/scale-sentry/internal/observer"
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
	// Clientset is the typed client used for the pods/log subresource —
	// controller-runtime's client does not support log streaming.
	Clientset kubernetes.Interface
	// LoadgenImage / ObserverImage are the container images for the
	// loadgen Job's load generator and observer sidecar.
	LoadgenImage  string
	ObserverImage string
	// ObserverServiceAccount is the ServiceAccount the Job pod runs as,
	// granting the observer its read + pods/exec permissions.
	ObserverServiceAccount string
	// observerLogFn overrides the observer-log read. Production leaves it
	// nil — the Clientset pods/log path is used. The integration suite
	// injects a stub because envtest runs no kubelet to serve logs.
	observerLogFn func(context.Context, *corev1.Pod) ([]byte, error)
}

//+kubebuilder:rbac:groups=validation.scale-sentry.ek.co,resources=scalevalidations,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups=validation.scale-sentry.ek.co,resources=scalevalidations/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=validation.scale-sentry.ek.co,resources=scalevalidations/finalizers,verbs=update
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods/log,verbs=get
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
	case jobConditionTrue(&job, batchv1.JobFailed):
		log.Info("loadgen job failed", "job", job.Name)
		return r.setPhase(ctx, &cr, PhaseError)
	case jobConditionTrue(&job, batchv1.JobComplete):
		return r.finishRun(ctx, &cr)
	default:
		return ctrl.Result{}, nil
	}
}

// finishRun collects the observer sidecar's Report from a completed Job,
// writes the measured results to status, and sets the terminal phase:
// Succeeded, or Failed when the SLA or traffic-integrity verdict failed.
func (r *ScaleValidationReconciler) finishRun(ctx context.Context, cr *v1alpha1.ScaleValidation) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	pod, err := r.findJobPod(ctx, cr)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pod == nil {
		log.Info("loadgen job pod not found; cannot collect observer report")
		return r.setPhase(ctx, cr, PhaseError)
	}
	if !observerTerminated(pod) {
		// The Job is Complete (loadgen exited) but the observer sidecar
		// is still finalizing. Poll until it terminates.
		return ctrl.Result{RequeueAfter: 5 * time.Second}, nil
	}

	readLog := r.observerLog
	if r.observerLogFn != nil {
		readLog = r.observerLogFn
	}
	raw, err := readLog(ctx, pod)
	if err != nil {
		log.Error(err, "read observer log")
		return r.setPhase(ctx, cr, PhaseError)
	}
	report, err := observer.ParseReportLog(raw)
	if err != nil {
		log.Error(err, "parse observer report")
		return r.setPhase(ctx, cr, PhaseError)
	}

	applyReport(cr, report)
	cr.Status.Phase = PhaseSucceeded
	if report.SLAStatus == observer.VerdictFail || report.TrafficIntegrity == observer.VerdictFail {
		cr.Status.Phase = PhaseFailed
	}
	if err := r.Status().Update(ctx, cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("write run results: %w", err)
	}
	log.Info("validation run finished",
		"phase", cr.Status.Phase, "sla", report.SLAStatus, "traffic", report.TrafficIntegrity)
	return ctrl.Result{}, nil
}

// findJobPod returns the pod created by the CR's loadgen Job, or nil.
func (r *ScaleValidationReconciler) findJobPod(ctx context.Context, cr *v1alpha1.ScaleValidation) (*corev1.Pod, error) {
	var pods corev1.PodList
	if err := r.List(ctx, &pods,
		client.InNamespace(cr.Namespace),
		client.MatchingLabels{loadgenForLabel: cr.Name}); err != nil {
		return nil, fmt.Errorf("list job pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return nil, nil
	}
	return &pods.Items[0], nil
}

// observerTerminated reports whether the observer native sidecar has
// exited. Native sidecars are reported among the init container statuses.
func observerTerminated(pod *corev1.Pod) bool {
	for _, cs := range pod.Status.InitContainerStatuses {
		if cs.Name == observerContainerName {
			return cs.State.Terminated != nil
		}
	}
	return false
}

// observerLog fetches the observer container's full log output.
func (r *ScaleValidationReconciler) observerLog(ctx context.Context, pod *corev1.Pod) ([]byte, error) {
	req := r.Clientset.CoreV1().Pods(pod.Namespace).
		GetLogs(pod.Name, &corev1.PodLogOptions{Container: observerContainerName})
	raw, err := req.DoRaw(ctx)
	if err != nil {
		return nil, fmt.Errorf("get observer log: %w", err)
	}
	return raw, nil
}

// applyReport copies the observer's measured results into the CR status.
func applyReport(cr *v1alpha1.ScaleValidation, report observer.Report) {
	cr.Status.Diagnostics = report.Diagnostics
	cr.Status.ScaleUpDuration = report.ScaleUpDuration
	cr.Status.SLAStatus = report.SLAStatus
	cr.Status.TrafficIntegrity = report.TrafficIntegrity
	cr.Status.TotalRequests = report.TotalRequests
	cr.Status.FailedRequests = report.FailedRequests
	cr.Status.FailureRate = report.FailureRate
}

// spawnJob creates the loadgen Job for cr and moves it to Running.
func (r *ScaleValidationReconciler) spawnJob(ctx context.Context, cr *v1alpha1.ScaleValidation) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	url, err := r.resolveTargetURL(ctx, cr)
	if err != nil {
		log.Error(err, "resolve target URL")
		return r.setPhase(ctx, cr, PhaseError)
	}

	job := r.buildLoadgenJob(cr, url)
	if err := controllerutil.SetControllerReference(cr, job, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("set owner reference: %w", err)
	}
	if err := r.Create(ctx, job); err != nil {
		return ctrl.Result{}, fmt.Errorf("create loadgen job: %w", err)
	}
	log.Info("spawned loadgen job", "job", job.Name, "url", url)

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
