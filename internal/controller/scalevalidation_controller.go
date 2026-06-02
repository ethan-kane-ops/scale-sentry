// Package controller holds the controller-runtime reconcilers that drive
// scale-sentry as a Kubernetes operator. The ScaleValidationReconciler runs
// a validation by spawning a loadgen Job and tracking its lifecycle; the
// DeploymentShadowReconciler auto-creates ScaleValidations for annotated
// Deployments.
//
// The reconcilers are the only place in the codebase that talk to the
// Kubernetes API, the analyzer and loadgen packages stay client-free.
package controller

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
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
	"github.com/ethan-kane-ops/scale-sentry/internal/metrics"
	"github.com/ethan-kane-ops/scale-sentry/internal/observer"
)

// Readiness gate constants. The loadgen Job is held back until the target
// Deployment has at least one ready replica, without this the Job dials a
// Service with no endpoints and the entire run wastes its SLA window on
// connection-refused errors before the workload is even up.
const (
	targetReadyPollInterval = 5 * time.Second
	targetReadyWaitTimeout  = 5 * time.Minute
)

// Lifecycle phases written to status.phase. Pending and Running are
// transient; Succeeded, Failed, and Error are terminal. Terminating is the
// transient phase while the finalizer drains child resources after the CR
// has a deletionTimestamp.
const (
	PhasePending     = "Pending"
	PhaseRunning     = "Running"
	PhaseSucceeded   = "Succeeded"
	PhaseFailed      = "Failed"
	PhaseError       = "Error"
	PhaseTerminating = "Terminating"
)

// scaleValidationFinalizer guards the CR from immediate deletion so the
// reconciler can tear down its child loadgen Job + observer sidecar pod
// before letting Kubernetes garbage-collect the CR.
const scaleValidationFinalizer = "validation.scale-sentry.ek.co/finalizer"

// childTerminationRequeue is how long to wait for a child Job + pods to
// fully clear after a Delete request before the finalizer is dropped.
const childTerminationRequeue = 2 * time.Second

// ScaleValidationReconciler reconciles a ScaleValidation object.
type ScaleValidationReconciler struct {
	client.Client
	Scheme *runtime.Scheme
	// Clientset is the typed client used for the pods/log subresource, // controller-runtime's client does not support log streaming.
	Clientset kubernetes.Interface
	// LoadgenImage / ObserverImage are the container images for the
	// loadgen Job's load generator and observer sidecar.
	LoadgenImage  string
	ObserverImage string
	// ObserverServiceAccount is the ServiceAccount the Job pod runs as,
	// granting the observer its read + pods/exec permissions.
	ObserverServiceAccount string
	// observerLogFn overrides the observer-log read. Production leaves it
	// nil, the Clientset pods/log path is used. The integration suite
	// injects a stub because envtest runs no kubelet to serve logs.
	observerLogFn func(context.Context, *corev1.Pod) ([]byte, error)
	// targetReadyTimeout overrides targetReadyWaitTimeout in tests so the
	// readiness-gate timeout path can be exercised without sleeping. Zero
	// means use the production default.
	targetReadyTimeout time.Duration
}

//+kubebuilder:rbac:groups=validation.scale-sentry.ek.co,resources=scalevalidations,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups=validation.scale-sentry.ek.co,resources=scalevalidations/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=validation.scale-sentry.ek.co,resources=scalevalidations/finalizers,verbs=update
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch
//+kubebuilder:rbac:groups="",resources=pods/log,verbs=get
//+kubebuilder:rbac:groups="",resources=configmaps,verbs=get
//+kubebuilder:rbac:groups=coordination.k8s.io,resources=leases,verbs=get;list;watch;create;update;patch;delete
//+kubebuilder:rbac:groups="",resources=events,verbs=create;patch

// Reconcile drives the ScaleValidation lifecycle: empty -> Pending -> spawn
// loadgen Job -> Running -> Succeeded/Error once the Job reaches a terminal
// condition. Terminal phases are not re-run. CRs carrying a
// deletionTimestamp run the finalizer cleanup path before yielding to GC.
func (r *ScaleValidationReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var cr v1alpha1.ScaleValidation
	if err := r.Get(ctx, req.NamespacedName, &cr); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}

	if !cr.DeletionTimestamp.IsZero() {
		return r.finalize(ctx, &cr)
	}
	if !controllerutil.ContainsFinalizer(&cr, scaleValidationFinalizer) {
		controllerutil.AddFinalizer(&cr, scaleValidationFinalizer)
		if err := r.Update(ctx, &cr); err != nil {
			return ctrl.Result{}, fmt.Errorf("add finalizer: %w", err)
		}
		// Continue this reconcile so the first observed phase update is
		// in the same pass. r.Update refreshes ResourceVersion in-place,
		// so the subsequent Status().Update below still applies cleanly.
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
		ready, readyReplicas, err := r.targetReady(ctx, &cr)
		if err != nil {
			return ctrl.Result{}, err
		}
		if !ready {
			timeout := r.targetReadyTimeout
			if timeout == 0 {
				timeout = targetReadyWaitTimeout
			}
			if time.Since(cr.CreationTimestamp.Time) > timeout {
				return r.failTargetNotReady(ctx, &cr, readyReplicas, timeout)
			}
			log.Info("target deployment not ready, requeueing",
				"target", cr.Spec.TargetRef.Name, "readyReplicas", readyReplicas)
			return ctrl.Result{RequeueAfter: targetReadyPollInterval}, nil
		}
		return r.spawnJob(ctx, &cr)
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("get loadgen job: %w", err)
	}

	switch {
	case jobConditionTrue(&job, batchv1.JobFailed):
		log.Info("loadgen job failed", "job", job.Name)
		metrics.RunsTotal.WithLabelValues(metrics.VerdictUnknown).Inc()
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
	recordRunMetrics(cr, report)
	log.Info("validation run finished",
		"phase", cr.Status.Phase, "sla", report.SLAStatus, "traffic", report.TrafficIntegrity)
	return ctrl.Result{}, nil
}

// recordRunMetrics emits the per-run Prometheus observations once the run
// has been written back to status. Safe to call with a nil ScaleUpDuration
// (the HPA react histogram is then skipped, not observed as zero).
func recordRunMetrics(cr *v1alpha1.ScaleValidation, report observer.Report) {
	metrics.RunsTotal.WithLabelValues(metrics.VerdictFromStatus(report.SLAStatus, report.TrafficIntegrity)).Inc()
	if cr.Status.LastRunTime != nil {
		metrics.RunDurationSeconds.Observe(time.Since(cr.Status.LastRunTime.Time).Seconds())
	}
	if report.ScaleUpDuration != nil {
		metrics.HPAReactSeconds.Observe(report.ScaleUpDuration.Seconds())
	}
	for _, d := range report.Diagnostics {
		metrics.DiagnosticAlertsTotal.WithLabelValues(d.Type, d.Severity).Inc()
	}
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

// targetReady reports whether the CR's target Deployment has at least one
// ready replica. A missing Deployment counts as not-ready, the user may
// have applied the CR before the workload, or the workload may still be
// rolling out, and is reported with readyReplicas=0 rather than as an
// error. The replica count is returned so the timeout diagnostic can
// distinguish "deployment missing" (0 ready) from "deployment exists but
// none healthy" (also 0 ready, but the surrounding events will explain why).
func (r *ScaleValidationReconciler) targetReady(ctx context.Context, cr *v1alpha1.ScaleValidation) (bool, int32, error) {
	var deploy appsv1.Deployment
	key := types.NamespacedName{Namespace: cr.Namespace, Name: cr.Spec.TargetRef.Name}
	if err := r.Get(ctx, key, &deploy); err != nil {
		if apierrors.IsNotFound(err) {
			return false, 0, nil
		}
		return false, 0, fmt.Errorf("get target deployment %s: %w", cr.Spec.TargetRef.Name, err)
	}
	return deploy.Status.ReadyReplicas >= 1, deploy.Status.ReadyReplicas, nil
}

// failTargetNotReady appends a Critical TargetNotReady diagnostic and moves
// the CR to PhaseError. Called when the target Deployment never reaches
// readyReplicas>=1 within the wait window, the loadgen run cannot proceed
// because there is nothing to send traffic to.
func (r *ScaleValidationReconciler) failTargetNotReady(ctx context.Context, cr *v1alpha1.ScaleValidation, readyReplicas int32, timeout time.Duration) (ctrl.Result, error) {
	cr.Status.Diagnostics = append(cr.Status.Diagnostics, v1alpha1.DiagnosticAlert{
		Type:     "TargetNotReady",
		Severity: "Critical",
		Message: fmt.Sprintf("target deployment %s/%s had %d ready replicas after waiting %s",
			cr.Namespace, cr.Spec.TargetRef.Name, readyReplicas, timeout),
		Recommendation: "ensure the target Deployment is healthy before applying the ScaleValidation",
	})
	metrics.RunsTotal.WithLabelValues(metrics.VerdictUnknown).Inc()
	metrics.DiagnosticAlertsTotal.WithLabelValues("TargetNotReady", "Critical").Inc()
	return r.setPhase(ctx, cr, PhaseError)
}

// spawnJob creates the loadgen Job for cr and moves it to Running.
func (r *ScaleValidationReconciler) spawnJob(ctx context.Context, cr *v1alpha1.ScaleValidation) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	url, err := r.resolveTargetURL(ctx, cr)
	if err != nil {
		log.Error(err, "resolve target URL")
		return r.setPhase(ctx, cr, PhaseError)
	}

	if terminal, err := r.validateTLSCABundle(ctx, cr); err != nil {
		return ctrl.Result{}, err
	} else if terminal {
		return ctrl.Result{}, nil
	}

	job, err := r.buildLoadgenJob(cr, url)
	if err != nil {
		log.Error(err, "build loadgen job")
		return r.setPhase(ctx, cr, PhaseError)
	}
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

// finalize handles the deletion path: surfaces a Terminating phase,
// requests deletion of the child loadgen Job (Background propagation, so
// pods follow), waits one short requeue for the apiserver to ack, then
// removes the finalizer so the CR is garbage-collected. Safe to re-run
// against a CR whose children were already gone before the user issued
// the delete.
func (r *ScaleValidationReconciler) finalize(ctx context.Context, cr *v1alpha1.ScaleValidation) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	if !controllerutil.ContainsFinalizer(cr, scaleValidationFinalizer) {
		return ctrl.Result{}, nil
	}

	var job batchv1.Job
	jobKey := types.NamespacedName{Namespace: cr.Namespace, Name: loadgenJobName(cr)}
	err := r.Get(ctx, jobKey, &job)
	switch {
	case apierrors.IsNotFound(err):
		// Job is gone; fall through to drop the finalizer.
	case err != nil:
		return ctrl.Result{}, fmt.Errorf("get loadgen job during finalize: %w", err)
	default:
		// Job still around. Surface Terminating phase for operator
		// visibility, request deletion, and requeue so the apiserver
		// has time to clear it before the finalizer drops.
		if cr.Status.Phase != PhaseTerminating {
			cr.Status.Phase = PhaseTerminating
			if err := r.Status().Update(ctx, cr); err != nil {
				return ctrl.Result{}, fmt.Errorf("set Terminating phase: %w", err)
			}
		}
		if job.DeletionTimestamp.IsZero() {
			bg := metav1.DeletePropagationBackground
			if err := r.Delete(ctx, &job, &client.DeleteOptions{PropagationPolicy: &bg}); err != nil && !apierrors.IsNotFound(err) {
				return ctrl.Result{}, fmt.Errorf("delete loadgen job: %w", err)
			}
			log.Info("requested loadgen job deletion during finalize", "job", job.Name)
		}
		return ctrl.Result{RequeueAfter: childTerminationRequeue}, nil
	}

	controllerutil.RemoveFinalizer(cr, scaleValidationFinalizer)
	if err := r.Update(ctx, cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("remove finalizer: %w", err)
	}
	log.Info("finalizer removed; CR ready for GC")
	return ctrl.Result{}, nil
}

// validateTLSCABundle ensures the CA-bundle ConfigMap referenced by the CR
// exists and carries the configured key. Returns terminal=true when the CR
// has been resolved to a terminal Error phase (the caller must stop), or
// terminal=false with a nil error when no CA bundle is configured or the
// configured one is valid.
func (r *ScaleValidationReconciler) validateTLSCABundle(ctx context.Context, cr *v1alpha1.ScaleValidation) (bool, error) {
	ref := caBundleRef(cr)
	if ref == nil {
		return false, nil
	}
	var cm corev1.ConfigMap
	key := types.NamespacedName{Namespace: cr.Namespace, Name: ref.Name}
	if err := r.Get(ctx, key, &cm); err != nil {
		if apierrors.IsNotFound(err) {
			return true, r.failTLSCABundle(ctx, cr,
				fmt.Sprintf("CA bundle ConfigMap %s/%s not found", cr.Namespace, ref.Name),
				"create the ConfigMap referenced by spec.target.tls.caBundle.configMapRef before applying the ScaleValidation")
		}
		return false, fmt.Errorf("get CA bundle configmap: %w", err)
	}
	if _, ok := cm.Data[ref.Key]; !ok {
		return true, r.failTLSCABundle(ctx, cr,
			fmt.Sprintf("CA bundle ConfigMap %s/%s missing key %q", cr.Namespace, ref.Name, ref.Key),
			"set the configured key in the ConfigMap, or update spec.target.tls.caBundle.configMapRef.key")
	}
	return false, nil
}

// failTLSCABundle appends a Critical TLSCABundleMissing diagnostic and
// transitions the CR to PhaseError.
func (r *ScaleValidationReconciler) failTLSCABundle(ctx context.Context, cr *v1alpha1.ScaleValidation, msg, rec string) error {
	cr.Status.Diagnostics = append(cr.Status.Diagnostics, v1alpha1.DiagnosticAlert{
		Type:           "TLSCABundleMissing",
		Severity:       "Critical",
		Message:        msg,
		Recommendation: rec,
	})
	metrics.RunsTotal.WithLabelValues(metrics.VerdictUnknown).Inc()
	metrics.DiagnosticAlertsTotal.WithLabelValues("TLSCABundleMissing", "Critical").Inc()
	_, err := r.setPhase(ctx, cr, PhaseError)
	return err
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
