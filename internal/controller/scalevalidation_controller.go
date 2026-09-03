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
	"errors"
	"fmt"
	"math"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"

	v1beta1 "github.com/ethan-kane-ops/scale-sentry/api/v1beta1"
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

// Lifecycle phases are defined on the API package: they are part of the
// contract a user reads off status.phase, not an internal detail of the
// reconciler. Aliased here so call sites stay short.
const (
	PhasePending     = v1beta1.PhasePending
	PhaseRunning     = v1beta1.PhaseRunning
	PhaseSucceeded   = v1beta1.PhaseSucceeded
	PhaseFailed      = v1beta1.PhaseFailed
	PhaseError       = v1beta1.PhaseError
	PhaseTerminating = v1beta1.PhaseTerminating
)

// scaleValidationFinalizer guards the CR from immediate deletion so the
// reconciler can tear down its child loadgen Job + observer sidecar pod
// before letting Kubernetes garbage-collect the CR.
const scaleValidationFinalizer = "validation.scale-sentry.ek.co/finalizer"

// childTerminationRequeue is how long to wait for a child Job + pods to
// fully clear after a Delete request before the finalizer is dropped.
const childTerminationRequeue = 2 * time.Second

// Event Reason constants written via Recorder. Kept PascalCase + short so
// `kubectl describe scalevalidation` renders them cleanly and external
// consumers (controllers watching Events, alert routers) can match on
// stable strings. Documented in docs/events.md.
const (
	EventReasonLoadgenJobCreated  = "LoadgenJobCreated"
	EventReasonLoadgenJobFailed   = "LoadgenJobFailed"
	EventReasonLoadgenJobVanished = "LoadgenJobVanished"
	EventReasonTargetReadyTimeout = "TargetReadyTimeout"
	EventReasonTargetUnresolvable = "TargetUnresolvable"
	EventReasonTLSCABundleMissing = "TLSCABundleMissing"
	EventReasonVerdictPass        = "VerdictPass"
	EventReasonVerdictFail        = "VerdictFail"
	EventReasonRunErrored         = "RunErrored"
	EventReasonFinalizerDraining  = "FinalizerDraining"
	EventReasonChaosInjected      = "ChaosInjected"
	EventReasonChaosSkipped       = "ChaosSkipped"
	EventReasonRunScheduled       = "RunScheduled"
	EventReasonScheduleInvalid    = "ScheduleInvalid"
	EventReasonSpecChanged        = "SpecChanged"
)

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
	// ImagePullSecrets are set on every loadgen Job pod. The controller's
	// own pull secrets come from its Deployment, but the Job pods it
	// creates are separate objects that pull the same images, so a private
	// registry needs them threaded through here too.
	ImagePullSecrets []string
	// Recorder publishes Events against the ScaleValidation CR so
	// `kubectl describe scalevalidation` narrates the run lifecycle.
	// Nil is tolerated (eventf no-ops) so callers may stub the recorder
	// out in unit tests that don't care about Events.
	Recorder record.EventRecorder
	// observerLogFn overrides the observer-log read. Production leaves it
	// nil, the Clientset pods/log path is used. The integration suite
	// injects a stub because envtest runs no kubelet to serve logs.
	observerLogFn func(context.Context, *corev1.Pod) ([]byte, error)
	// targetReadyTimeout overrides targetReadyWaitTimeout in tests so the
	// readiness-gate timeout path can be exercised without sleeping. Zero
	// means use the production default.
	targetReadyTimeout time.Duration
	// nowFn overrides the clock used for schedule arithmetic so the
	// scheduling tests can advance time deterministically instead of
	// sleeping through real cron intervals. Production leaves it nil.
	nowFn func() time.Time
}

// now is the reconciler's clock, overridable in tests. It is used only
// for schedule arithmetic; everything else takes timestamps from the
// apiserver.
func (r *ScaleValidationReconciler) now() time.Time {
	if r.nowFn != nil {
		return r.nowFn()
	}
	return time.Now()
}

// eventf emits an Event against cr, no-oping if Recorder is unset.
func (r *ScaleValidationReconciler) eventf(cr *v1beta1.ScaleValidation, eventType, reason, format string, args ...any) {
	if r.Recorder == nil {
		return
	}
	r.Recorder.Eventf(cr, eventType, reason, format, args...)
}

//+kubebuilder:rbac:groups=validation.scale-sentry.ek.co,resources=scalevalidations,verbs=get;list;watch;create;update;patch
//+kubebuilder:rbac:groups=validation.scale-sentry.ek.co,resources=scalevalidations/status,verbs=get;update;patch
//+kubebuilder:rbac:groups=validation.scale-sentry.ek.co,resources=scalevalidations/finalizers,verbs=update
//+kubebuilder:rbac:groups=batch,resources=jobs,verbs=get;list;watch;create;delete
//+kubebuilder:rbac:groups=apps,resources=deployments;statefulsets;replicasets,verbs=get;list;watch
//+kubebuilder:rbac:groups=apps,resources=deployments/scale;statefulsets/scale;replicasets/scale,verbs=get
//+kubebuilder:rbac:groups="",resources=pods,verbs=get;list;watch;delete
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

	var cr v1beta1.ScaleValidation
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
		// One-shot validations stop here, as they always have. A
		// scheduled one parks until its next due time or starts it.
		return r.reconcileTerminal(ctx, &cr)
	case "":
		if _, err := parseSchedule(&cr); err != nil {
			return r.failScheduleInvalid(ctx, &cr, err)
		}
		return r.setPhase(ctx, &cr, PhasePending)
	}

	var job batchv1.Job
	jobKey := types.NamespacedName{Namespace: cr.Namespace, Name: loadgenJobName(&cr)}
	err := r.Get(ctx, jobKey, &job)
	switch {
	case apierrors.IsNotFound(err):
		if cr.Status.Phase == PhaseRunning {
			log.Info("loadgen job vanished before completion", "job", jobKey.Name)
			r.eventf(&cr, corev1.EventTypeWarning, EventReasonLoadgenJobVanished,
				"loadgen Job %s disappeared while phase=Running", jobKey.Name)
			return r.setTerminalPhase(ctx, &cr, PhaseError, FinishedReasonLoadgenJobVanished,
				fmt.Sprintf("loadgen Job %s disappeared while phase=Running", jobKey.Name))
		}
		ready, readyReplicas, err := r.targetReady(ctx, &cr)
		if errors.Is(err, errTargetUnresolvable) {
			return ctrl.Result{}, r.failTargetUnresolvable(ctx, &cr, err)
		}
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
			log.Info("target workload not ready, requeueing",
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
		r.eventf(&cr, corev1.EventTypeWarning, EventReasonLoadgenJobFailed,
			"loadgen Job %s reached condition Failed=True", job.Name)
		metrics.RunsTotal.WithLabelValues(cr.Namespace, cr.Name, metrics.VerdictUnknown).Inc()
		return r.setTerminalPhase(ctx, &cr, PhaseError, FinishedReasonLoadgenJobFailed,
			fmt.Sprintf("loadgen Job %s reached condition Failed=True", job.Name))
	case jobConditionTrue(&job, batchv1.JobComplete):
		return r.finishRun(ctx, &cr)
	default:
		// Job in flight: the only mid-run duty is chaos injection.
		return r.maybeInjectDisruption(ctx, &cr)
	}
}

// finishRun collects the observer sidecar's Report from a completed Job,
// writes the measured results to status, and sets the terminal phase:
// Succeeded, or Failed when the SLA or traffic-integrity verdict failed.
func (r *ScaleValidationReconciler) finishRun(ctx context.Context, cr *v1beta1.ScaleValidation) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	pod, err := r.findJobPod(ctx, cr)
	if err != nil {
		return ctrl.Result{}, err
	}
	if pod == nil {
		log.Info("loadgen job pod not found; cannot collect observer report")
		r.eventf(cr, corev1.EventTypeWarning, EventReasonRunErrored,
			"loadgen Job %s completed but no pod found to read observer report from", loadgenJobName(cr))
		return r.setTerminalPhase(ctx, cr, PhaseError, FinishedReasonObserverReportUnreadable,
			fmt.Sprintf("loadgen Job %s completed but no pod was found to read the observer report from", loadgenJobName(cr)))
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
		r.eventf(cr, corev1.EventTypeWarning, EventReasonRunErrored,
			"reading observer log from pod %s failed: %v", pod.Name, err)
		return r.setTerminalPhase(ctx, cr, PhaseError, FinishedReasonObserverReportUnreadable,
			fmt.Sprintf("reading the observer log from pod %s failed: %v", pod.Name, err))
	}
	report, err := observer.ParseReportLog(raw)
	if err != nil {
		log.Error(err, "parse observer report")
		r.eventf(cr, corev1.EventTypeWarning, EventReasonRunErrored,
			"parsing observer report from pod %s failed: %v", pod.Name, err)
		return r.setTerminalPhase(ctx, cr, PhaseError, FinishedReasonObserverReportUnreadable,
			fmt.Sprintf("parsing the observer report from pod %s failed: %v", pod.Name, err))
	}

	applyReport(cr, report)
	phase, reason, message := finishedVerdict(report.SLAStatus, report.TrafficIntegrity,
		report.TotalRequests, report.FailedRequests)
	cr.Status.Phase = phase
	markFinished(cr, reason, message)
	appendRunHistory(cr)
	if err := r.Status().Update(ctx, cr); err != nil {
		return ctrl.Result{}, fmt.Errorf("write run results: %w", err)
	}
	recordRunMetrics(cr, report)
	emitVerdictEvent(r, cr, report)
	log.Info("validation run finished",
		"phase", cr.Status.Phase, "sla", report.SLAStatus, "traffic", report.TrafficIntegrity)
	return ctrl.Result{}, nil
}

// emitVerdictEvent fires the terminal Event for a finished run. Pass on
// Succeeded (Normal VerdictPass), Warning VerdictFail otherwise. The
// message embeds the top diagnostic so `kubectl describe` shows the
// "why" without a second roundtrip.
func emitVerdictEvent(r *ScaleValidationReconciler, cr *v1beta1.ScaleValidation, report observer.Report) {
	if cr.Status.Phase == PhaseSucceeded {
		r.eventf(cr, corev1.EventTypeNormal, EventReasonVerdictPass,
			"SLA=%s traffic=%s requests=%d failed=%d",
			report.SLAStatus, report.TrafficIntegrity, report.TotalRequests, report.FailedRequests)
		return
	}
	top := topDiagnostic(report.Diagnostics)
	r.eventf(cr, corev1.EventTypeWarning, EventReasonVerdictFail,
		"SLA=%s traffic=%s failed=%d top diagnostic: %s",
		report.SLAStatus, report.TrafficIntegrity, report.FailedRequests, top)
}

// topDiagnostic returns a short string describing the highest-severity
// diagnostic in diags, suitable for embedding in an Event message. Falls
// back to "<none>" when the run failed but no diagnostic was attached.
func topDiagnostic(diags []v1beta1.DiagnosticAlert) string {
	if len(diags) == 0 {
		return "<none>"
	}
	rank := map[v1beta1.Severity]int{v1beta1.SeverityCritical: 3, v1beta1.SeverityWarning: 2, v1beta1.SeverityInfo: 1}
	best := diags[0]
	for _, d := range diags[1:] {
		if rank[d.Severity] > rank[best.Severity] {
			best = d
		}
	}
	return fmt.Sprintf("%s (%s)", best.Type, best.Severity)
}

// recordRunMetrics emits the per-run Prometheus observations once the run
// has been written back to status. Safe to call with a nil ScaleUpDuration
// (the HPA react histogram is then skipped, not observed as zero).
func recordRunMetrics(cr *v1beta1.ScaleValidation, report observer.Report) {
	metrics.RunsTotal.WithLabelValues(cr.Namespace, cr.Name,
		metrics.VerdictFromStatus(string(report.SLAStatus), string(report.TrafficIntegrity))).Inc()
	if cr.Status.LastRunTime != nil {
		metrics.RunDurationSeconds.WithLabelValues(cr.Namespace, cr.Name).
			Observe(time.Since(cr.Status.LastRunTime.Time).Seconds())
	}
	if report.ScaleUpDuration != nil {
		metrics.HPAReactSeconds.WithLabelValues(cr.Namespace, cr.Name).
			Observe(report.ScaleUpDuration.Seconds())
	}
	for _, d := range report.Diagnostics {
		metrics.DiagnosticAlertsTotal.WithLabelValues(cr.Namespace, cr.Name, d.Type, string(d.Severity)).Inc()
	}
}

// findJobPod returns the pod created by the CR's loadgen Job, or nil.
func (r *ScaleValidationReconciler) findJobPod(ctx context.Context, cr *v1beta1.ScaleValidation) (*corev1.Pod, error) {
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
func applyReport(cr *v1beta1.ScaleValidation, report observer.Report) {
	cr.Status.Diagnostics = report.Diagnostics
	cr.Status.ScaleUpDuration = report.ScaleUpDuration
	cr.Status.SLAStatus = report.SLAStatus
	cr.Status.TrafficIntegrity = report.TrafficIntegrity
	cr.Status.TotalRequests = report.TotalRequests
	cr.Status.FailedRequests = report.FailedRequests
	cr.Status.FailureRateBasisPoints = failureRateBasisPoints(report.FailureRate)
}

// failureRateBasisPoints converts the observer's float ratio into the
// integer basis points the API carries. Rounded rather than truncated, so
// a real but tiny failure rate does not read as a clean zero, and clamped
// because a malformed report should not wrap the field negative.
func failureRateBasisPoints(rate float64) int32 {
	if rate <= 0 {
		return 0
	}
	bp := math.Round(rate * v1beta1.FailureRateScale)
	if bp > math.MaxInt32 {
		return math.MaxInt32
	}
	return int32(bp)
}

// appendRunHistory records the just-finalized run (cr.Status.Phase and the
// fields applyReport just set) at the front of History, newest first, and
// trims to RunHistoryLimit. Must run after applyReport and the terminal
// Phase assignment, before the status write, so both land in the same
// Status().Update call.
func appendRunHistory(cr *v1beta1.ScaleValidation) {
	entry := v1beta1.RunSummary{
		FinishedAt:             metav1.Now(),
		Phase:                  cr.Status.Phase,
		SLAStatus:              cr.Status.SLAStatus,
		TrafficIntegrity:       cr.Status.TrafficIntegrity,
		FailureRateBasisPoints: cr.Status.FailureRateBasisPoints,
	}
	cr.Status.History = append([]v1beta1.RunSummary{entry}, cr.Status.History...)
	if len(cr.Status.History) > v1beta1.RunHistoryLimit {
		cr.Status.History = cr.Status.History[:v1beta1.RunHistoryLimit]
	}
}

// targetReady reports whether the CR's target workload has at least one
// ready replica. The workload is whatever spec.targetRef names, resolved
// through its scale subresource, so a StatefulSet target is counted as a
// StatefulSet rather than silently probed as a Deployment.
//
// A missing workload counts as not-ready, the user may have applied the CR
// before the workload, or the workload may still be rolling out, and is
// reported with readyReplicas=0 rather than as an error. An unresolvable
// kind is a different case and surfaces as errTargetUnresolvable, which
// the caller turns into a terminal diagnostic. The replica count is
// returned so the timeout diagnostic can distinguish "workload missing"
// (0 ready) from "workload exists but none healthy" (also 0 ready, but the
// surrounding events will explain why).
func (r *ScaleValidationReconciler) targetReady(ctx context.Context, cr *v1beta1.ScaleValidation) (bool, int32, error) {
	return r.targetPodsReady(ctx, cr)
}

// failTargetUnresolvable appends a Critical TargetUnsupported diagnostic
// and moves the CR to PhaseError. Called when spec.targetRef names a kind
// this cluster does not serve, one the manager has no RBAC for, or a
// workload with no usable scale subresource. Requeueing would never
// succeed, so the run is failed immediately with the kind named in the
// message rather than left to time out against the readiness gate.
func (r *ScaleValidationReconciler) failTargetUnresolvable(ctx context.Context, cr *v1beta1.ScaleValidation, cause error) error {
	ref := cr.Spec.TargetRef
	msg := fmt.Sprintf("cannot resolve targetRef %s/%s %s: %v", ref.APIVersion, ref.Kind, ref.Name, cause)
	cr.Status.Diagnostics = append(cr.Status.Diagnostics, v1beta1.DiagnosticAlert{
		Type:     "TargetUnsupported",
		Severity: "Critical",
		Message:  msg,
		Recommendation: "check spec.targetRef.apiVersion and .kind name a scalable workload that exists in this cluster, " +
			"and that the manager ClusterRole grants read access to it and its scale subresource",
	})
	r.eventf(cr, corev1.EventTypeWarning, EventReasonTargetUnresolvable, "%s", msg)
	metrics.RunsTotal.WithLabelValues(cr.Namespace, cr.Name, metrics.VerdictUnknown).Inc()
	metrics.DiagnosticAlertsTotal.WithLabelValues(cr.Namespace, cr.Name, "TargetUnsupported", "Critical").Inc()
	_, err := r.setTerminalPhase(ctx, cr, PhaseError, FinishedReasonTargetUnsupported, msg)
	return err
}

// failTargetNotReady appends a Critical TargetNotReady diagnostic and moves
// the CR to PhaseError. Called when the target Deployment never reaches
// readyReplicas>=1 within the wait window, the loadgen run cannot proceed
// because there is nothing to send traffic to.
func (r *ScaleValidationReconciler) failTargetNotReady(ctx context.Context, cr *v1beta1.ScaleValidation, readyReplicas int32, timeout time.Duration) (ctrl.Result, error) {
	cr.Status.Diagnostics = append(cr.Status.Diagnostics, v1beta1.DiagnosticAlert{
		Type:     "TargetNotReady",
		Severity: "Critical",
		Message: fmt.Sprintf("target deployment %s/%s had %d ready replicas after waiting %s",
			cr.Namespace, cr.Spec.TargetRef.Name, readyReplicas, timeout),
		Recommendation: "ensure the target Deployment is healthy before applying the ScaleValidation",
	})
	r.eventf(cr, corev1.EventTypeWarning, EventReasonTargetReadyTimeout,
		"target Deployment %s/%s had %d ready replicas after waiting %s",
		cr.Namespace, cr.Spec.TargetRef.Name, readyReplicas, timeout)
	metrics.RunsTotal.WithLabelValues(cr.Namespace, cr.Name, metrics.VerdictUnknown).Inc()
	metrics.DiagnosticAlertsTotal.WithLabelValues(cr.Namespace, cr.Name, "TargetNotReady", "Critical").Inc()
	return r.setTerminalPhase(ctx, cr, PhaseError, FinishedReasonTargetNotReady,
		fmt.Sprintf("target %s %s had %d ready replicas after waiting %s",
			cr.Spec.TargetRef.Kind, cr.Spec.TargetRef.Name, readyReplicas, timeout))
}

// spawnJob creates the loadgen Job for cr and moves it to Running.
func (r *ScaleValidationReconciler) spawnJob(ctx context.Context, cr *v1beta1.ScaleValidation) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	url, err := r.resolveTargetURL(ctx, cr)
	if errors.Is(err, errTargetUnresolvable) {
		return ctrl.Result{}, r.failTargetUnresolvable(ctx, cr, err)
	}
	if err != nil {
		log.Error(err, "resolve target URL")
		return r.setTerminalPhase(ctx, cr, PhaseError, FinishedReasonTargetURLUnresolved,
			fmt.Sprintf("could not resolve the target URL: %v", err))
	}

	if terminal, err := r.validateTLSCABundle(ctx, cr); err != nil {
		return ctrl.Result{}, err
	} else if terminal {
		return ctrl.Result{}, nil
	}

	job, err := r.buildLoadgenJob(cr, url)
	if errors.Is(err, errTargetUnresolvable) {
		return ctrl.Result{}, r.failTargetUnresolvable(ctx, cr, err)
	}
	if err != nil {
		log.Error(err, "build loadgen job")
		return r.setTerminalPhase(ctx, cr, PhaseError, FinishedReasonJobBuildFailed,
			fmt.Sprintf("could not build the loadgen Job: %v", err))
	}
	if err := controllerutil.SetControllerReference(cr, job, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("set owner reference: %w", err)
	}
	if err := r.Create(ctx, job); err != nil {
		return ctrl.Result{}, fmt.Errorf("create loadgen job: %w", err)
	}
	log.Info("spawned loadgen job", "job", job.Name, "url", url)
	r.eventf(cr, corev1.EventTypeNormal, EventReasonLoadgenJobCreated,
		"loadgen Job %s created against %s", job.Name, url)

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
func (r *ScaleValidationReconciler) finalize(ctx context.Context, cr *v1beta1.ScaleValidation) (ctrl.Result, error) {
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
			r.eventf(cr, corev1.EventTypeNormal, EventReasonFinalizerDraining,
				"requested deletion of loadgen Job %s during CR finalize", job.Name)
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
func (r *ScaleValidationReconciler) validateTLSCABundle(ctx context.Context, cr *v1beta1.ScaleValidation) (bool, error) {
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
func (r *ScaleValidationReconciler) failTLSCABundle(ctx context.Context, cr *v1beta1.ScaleValidation, msg, rec string) error {
	cr.Status.Diagnostics = append(cr.Status.Diagnostics, v1beta1.DiagnosticAlert{
		Type:           "TLSCABundleMissing",
		Severity:       "Critical",
		Message:        msg,
		Recommendation: rec,
	})
	r.eventf(cr, corev1.EventTypeWarning, EventReasonTLSCABundleMissing, "%s", msg)
	metrics.RunsTotal.WithLabelValues(cr.Namespace, cr.Name, metrics.VerdictUnknown).Inc()
	metrics.DiagnosticAlertsTotal.WithLabelValues(cr.Namespace, cr.Name, "TLSCABundleMissing", "Critical").Inc()
	_, err := r.setTerminalPhase(ctx, cr, PhaseError, FinishedReasonTLSCABundleMissing, msg)
	return err
}

// setPhase persists phase to status.phase via the status subresource.
func (r *ScaleValidationReconciler) setPhase(ctx context.Context, cr *v1beta1.ScaleValidation, phase v1beta1.Phase) (ctrl.Result, error) {
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
		For(&v1beta1.ScaleValidation{}).
		Owns(&batchv1.Job{}).
		Named("scalevalidation").
		Complete(r)
}
