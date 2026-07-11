//go:build envtest

package controller

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
	"github.com/ethan-kane-ops/scale-sentry/internal/observer"
)

// --- helpers ---------------------------------------------------------------

func newTestNamespace(t *testing.T) string {
	t.Helper()
	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "svtest-"}}
	if err := k8sClient.Create(context.Background(), ns); err != nil {
		t.Fatalf("create namespace: %v", err)
	}
	return ns.Name
}

// fakeRecorderBuffer is the per-reconciler Event channel size. Generous so
// no test ever fills it and silently drops Events.
const fakeRecorderBuffer = 64

func newReconciler() *ScaleValidationReconciler {
	return &ScaleValidationReconciler{
		Client:                 k8sClient,
		Scheme:                 testScheme,
		LoadgenImage:           "test/loadgen:v1",
		ObserverImage:          "test/observer:v1",
		ObserverServiceAccount: "scale-sentry-observer",
		Recorder:               record.NewFakeRecorder(fakeRecorderBuffer),
	}
}

// drainEventReasons reads every Event currently on the FakeRecorder's
// channel and returns just the Reason field. FakeRecorder formats each
// Event as "<type> <reason> <message>", so a space-split picks Reason out.
func drainEventReasons(t *testing.T, r *ScaleValidationReconciler) []string {
	t.Helper()
	fr, ok := r.Recorder.(*record.FakeRecorder)
	if !ok {
		t.Fatalf("recorder is not a FakeRecorder: %T", r.Recorder)
	}
	var reasons []string
	for {
		select {
		case ev := <-fr.Events:
			parts := strings.SplitN(ev, " ", 3)
			if len(parts) >= 2 {
				reasons = append(reasons, parts[1])
			}
		default:
			return reasons
		}
	}
}

func newScaleValidation(ns, name string, mod func(*v1alpha1.ScaleValidation)) *v1alpha1.ScaleValidation {
	cr := &v1alpha1.ScaleValidation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1alpha1.ScaleValidationSpec{
			TargetRef: v1alpha1.CrossVersionObjectReference{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "app",
			},
			SLA:    metav1.Duration{Duration: time.Minute},
			Target: v1alpha1.TargetConfig{Mode: "ServiceDefault", Port: 8080, NetworkPath: "ClusterIP"},
			Load:   v1alpha1.LoadConfig{BaseRPS: 100, ConcurrencyFactor: 10},
		},
	}
	if mod != nil {
		mod(cr)
	}
	return cr
}

// newTargetDeployment builds a valid Deployment; with withProbe it carries
// an HTTPGet readiness probe on /healthz:9000.
func newTargetDeployment(ns, name string, withProbe bool) *appsv1.Deployment {
	labels := map[string]string{"app": name}
	c := corev1.Container{Name: "web", Image: "web:1"}
	if withProbe {
		c.ReadinessProbe = &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
			HTTPGet: &corev1.HTTPGetAction{Path: "/healthz", Port: intstr.FromInt32(9000)},
		}}
	}
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: appsv1.DeploymentSpec{
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{c}},
			},
		},
	}
}

func mustCreate(t *testing.T, obj client.Object) {
	t.Helper()
	if err := k8sClient.Create(context.Background(), obj); err != nil {
		t.Fatalf("create %T: %v", obj, err)
	}
}

// mustCreateReadyTarget creates the target Deployment the CR points at and
// patches its status so readyReplicas>=1. The reconciler's readiness gate
// holds the loadgen Job back until this is true, so every test that wants
// to drive the CR past Pending needs it.
func mustCreateReadyTarget(t *testing.T, ns, name string) {
	t.Helper()
	deploy := newTargetDeployment(ns, name, false)
	mustCreate(t, deploy)
	deploy.Status.Replicas = 1
	deploy.Status.ReadyReplicas = 1
	deploy.Status.AvailableReplicas = 1
	if err := k8sClient.Status().Update(context.Background(), deploy); err != nil {
		t.Fatalf("set deployment status: %v", err)
	}
}

func reconcileCR(t *testing.T, r *ScaleValidationReconciler, ns, name string) {
	t.Helper()
	_, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: name},
	})
	if err != nil {
		t.Fatalf("reconcile %s/%s: %v", ns, name, err)
	}
}

func getCR(t *testing.T, ns, name string) *v1alpha1.ScaleValidation {
	t.Helper()
	var cr v1alpha1.ScaleValidation
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: name}, &cr); err != nil {
		t.Fatalf("get ScaleValidation %s/%s: %v", ns, name, err)
	}
	return &cr
}

func getJob(t *testing.T, ns, name string) *batchv1.Job {
	t.Helper()
	var job batchv1.Job
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: name}, &job); err != nil {
		t.Fatalf("get Job %s/%s: %v", ns, name, err)
	}
	return &job
}

func jobCond(typ batchv1.JobConditionType, now metav1.Time) batchv1.JobCondition {
	return batchv1.JobCondition{
		Type:               typ,
		Status:             corev1.ConditionTrue,
		LastProbeTime:      now,
		LastTransitionTime: now,
	}
}

// completeJob drives a Job into the terminal Complete state. The apiserver
// requires the SuccessCriteriaMet companion condition plus start/completion
// timestamps before it will accept Complete=True.
func completeJob(t *testing.T, ns, name string) {
	t.Helper()
	job := getJob(t, ns, name)
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.CompletionTime = &now
	job.Status.Conditions = []batchv1.JobCondition{
		jobCond(batchv1.JobSuccessCriteriaMet, now),
		jobCond(batchv1.JobComplete, now),
	}
	if err := k8sClient.Status().Update(context.Background(), job); err != nil {
		t.Fatalf("complete job: %v", err)
	}
}

// failJob drives a Job into the terminal Failed state, with the
// FailureTarget companion condition the apiserver requires.
func failJob(t *testing.T, ns, name string) {
	t.Helper()
	job := getJob(t, ns, name)
	now := metav1.Now()
	job.Status.StartTime = &now
	job.Status.Conditions = []batchv1.JobCondition{
		jobCond(batchv1.JobFailureTarget, now),
		jobCond(batchv1.JobFailed, now),
	}
	if err := k8sClient.Status().Update(context.Background(), job); err != nil {
		t.Fatalf("fail job: %v", err)
	}
}

// createObserverPod creates a Job pod whose observer sidecar has terminated.
func createObserverPod(t *testing.T, ns, crName string) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name:      crName + "-loadgen-pod",
			Namespace: ns,
			Labels:    map[string]string{loadgenForLabel: crName},
		},
		Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "loadgen", Image: "x"}}},
	}
	mustCreate(t, pod)
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name:  observerContainerName,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
	}}
	if err := k8sClient.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("set pod status: %v", err)
	}
}

// --- tests -----------------------------------------------------------------

func TestIntegration_PendingThenRunning(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, newScaleValidation(ns, "run", nil))

	reconcileCR(t, r, ns, "run")
	if got := getCR(t, ns, "run").Status.Phase; got != PhasePending {
		t.Fatalf("after first reconcile phase = %q, want Pending", got)
	}

	reconcileCR(t, r, ns, "run")
	if got := getCR(t, ns, "run").Status.Phase; got != PhaseRunning {
		t.Fatalf("after second reconcile phase = %q, want Running", got)
	}

	job := getJob(t, ns, "run-loadgen")
	if len(job.OwnerReferences) == 0 || job.OwnerReferences[0].Name != "run" {
		t.Errorf("job owner references = %v, want owner run", job.OwnerReferences)
	}
	if len(job.Spec.Template.Spec.InitContainers) != 1 {
		t.Errorf("job has %d init containers, want 1 (observer sidecar)",
			len(job.Spec.Template.Spec.InitContainers))
	}
}

func TestIntegration_JobVanishedDuringRun(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	mustCreate(t, newScaleValidation(ns, "run", nil))

	// Force the CR into Running without ever spawning a Job, the
	// reconciler must detect the missing Job and fail the run. This
	// sidesteps depending on envtest GC behaviour for a real Job delete.
	cr := getCR(t, ns, "run")
	cr.Status.Phase = PhaseRunning
	if err := k8sClient.Status().Update(context.Background(), cr); err != nil {
		t.Fatalf("force Running: %v", err)
	}

	reconcileCR(t, r, ns, "run")
	if got := getCR(t, ns, "run").Status.Phase; got != PhaseError {
		t.Fatalf("phase = %q, want Error after the job vanished", got)
	}
}

func TestIntegration_JobFailedConditionIsError(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, newScaleValidation(ns, "run", nil))

	reconcileCR(t, r, ns, "run")
	reconcileCR(t, r, ns, "run")
	failJob(t, ns, "run-loadgen")

	reconcileCR(t, r, ns, "run")
	if got := getCR(t, ns, "run").Status.Phase; got != PhaseError {
		t.Fatalf("phase = %q, want Error after job failed", got)
	}
}

func TestIntegration_FinishRunSucceeded(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	line, err := observer.MarshalReportLine(observer.Report{
		SLAStatus:        observer.VerdictPass,
		TrafficIntegrity: observer.VerdictPass,
		TotalRequests:    5000,
		FailedRequests:   3,
		Diagnostics: []v1alpha1.DiagnosticAlert{
			{Type: "CPUThrottling", Severity: "Warning", Message: "throttled"},
		},
	})
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	r.observerLogFn = func(context.Context, *corev1.Pod) ([]byte, error) {
		return []byte("observer: starting\n" + line + "\n"), nil
	}

	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, newScaleValidation(ns, "run", nil))
	reconcileCR(t, r, ns, "run")
	reconcileCR(t, r, ns, "run")
	completeJob(t, ns, "run-loadgen")
	createObserverPod(t, ns, "run")

	reconcileCR(t, r, ns, "run")
	got := getCR(t, ns, "run")
	if got.Status.Phase != PhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded", got.Status.Phase)
	}
	if got.Status.TotalRequests != 5000 || got.Status.FailedRequests != 3 {
		t.Errorf("metrics = %d/%d, want 5000/3", got.Status.TotalRequests, got.Status.FailedRequests)
	}
	if got.Status.SLAStatus != "Pass" || got.Status.TrafficIntegrity != "Pass" {
		t.Errorf("verdicts = %s/%s, want Pass/Pass", got.Status.SLAStatus, got.Status.TrafficIntegrity)
	}
	if len(got.Status.Diagnostics) != 1 || got.Status.Diagnostics[0].Type != "CPUThrottling" {
		t.Errorf("diagnostics = %+v, want one CPUThrottling alert", got.Status.Diagnostics)
	}
}

func TestIntegration_FinishRunFailedVerdict(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	line, err := observer.MarshalReportLine(observer.Report{
		SLAStatus:        observer.VerdictFail,
		TrafficIntegrity: observer.VerdictPass,
	})
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	r.observerLogFn = func(context.Context, *corev1.Pod) ([]byte, error) {
		return []byte(line + "\n"), nil
	}

	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, newScaleValidation(ns, "run", nil))
	reconcileCR(t, r, ns, "run")
	reconcileCR(t, r, ns, "run")
	completeJob(t, ns, "run-loadgen")
	createObserverPod(t, ns, "run")

	reconcileCR(t, r, ns, "run")
	if got := getCR(t, ns, "run").Status.Phase; got != PhaseFailed {
		t.Fatalf("phase = %q, want Failed when the SLA verdict failed", got)
	}
}

func TestIntegration_ShadowControllerCreatesCR(t *testing.T) {
	ns := newTestNamespace(t)
	deploy := newTargetDeployment(ns, "web", false)
	deploy.Annotations = map[string]string{shadowEnableAnnotation: "true"}
	mustCreate(t, deploy)

	sr := &DeploymentShadowReconciler{Client: k8sClient, Scheme: testScheme}
	if _, err := sr.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: "web"},
	}); err != nil {
		t.Fatalf("shadow reconcile: %v", err)
	}

	var sv v1alpha1.ScaleValidation
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: "web-shadow"}, &sv); err != nil {
		t.Fatalf("shadow ScaleValidation not created: %v", err)
	}
	if sv.Spec.TargetRef.Name != "web" {
		t.Errorf("shadow targetRef = %q, want web", sv.Spec.TargetRef.Name)
	}
	if len(sv.OwnerReferences) == 0 || sv.OwnerReferences[0].Name != "web" {
		t.Errorf("shadow owner references = %v, want owner web", sv.OwnerReferences)
	}
}

func TestIntegration_AutoDiscoverProbe(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	deploy := newTargetDeployment(ns, "app", true)
	mustCreate(t, deploy)
	deploy.Status.Replicas = 1
	deploy.Status.ReadyReplicas = 1
	deploy.Status.AvailableReplicas = 1
	if err := k8sClient.Status().Update(context.Background(), deploy); err != nil {
		t.Fatalf("set deployment status: %v", err)
	}
	mustCreate(t, newScaleValidation(ns, "run", func(cr *v1alpha1.ScaleValidation) {
		cr.Spec.Target.Mode = "AutoDiscoverProbe"
	}))

	reconcileCR(t, r, ns, "run")
	reconcileCR(t, r, ns, "run")

	job := getJob(t, ns, "run-loadgen")
	args := job.Spec.Template.Spec.Containers[0].Args
	i := slices.Index(args, "--url")
	if i < 0 || i+1 >= len(args) {
		t.Fatalf("--url flag missing from loadgen args %v", args)
	}
	// The discovered probe port (9000) and path (/healthz) win over the spec.
	if url := args[i+1]; !strings.HasSuffix(url, ":9000/healthz") {
		t.Errorf("loadgen --url = %q, want the discovered :9000/healthz endpoint", url)
	}
}

// TestIntegration_TargetMissing_HoldsPending exercises the readiness gate
// for the "user applied the CR before the Deployment exists" case: the
// reconciler must not spawn the loadgen Job, must not error, and must
// leave the CR in Pending so the next requeue can pick the workload up
// once it lands.
func TestIntegration_TargetMissing_HoldsPending(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	mustCreate(t, newScaleValidation(ns, "run", nil))

	reconcileCR(t, r, ns, "run")
	reconcileCR(t, r, ns, "run")

	if got := getCR(t, ns, "run").Status.Phase; got != PhasePending {
		t.Fatalf("phase = %q, want Pending while target is missing", got)
	}
	var job batchv1.Job
	err := k8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: "run-loadgen"}, &job)
	if err == nil {
		t.Fatal("loadgen Job created while target Deployment is missing")
	}
}

// TestIntegration_TargetUnready_ThenReady covers the typical sequence: the
// Deployment exists but is still rolling out (readyReplicas=0), and only
// once it becomes ready does the controller spawn the Job. This is the
// regression test for the live bug observed in `just dev-up`, where the
// Job was firing against zero endpoints.
func TestIntegration_TargetUnready_ThenReady(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	deploy := newTargetDeployment(ns, "app", false)
	mustCreate(t, deploy)
	// Deployment exists, no ready replicas yet.
	mustCreate(t, newScaleValidation(ns, "run", nil))

	reconcileCR(t, r, ns, "run")
	reconcileCR(t, r, ns, "run")
	if got := getCR(t, ns, "run").Status.Phase; got != PhasePending {
		t.Fatalf("phase = %q, want Pending while readyReplicas=0", got)
	}

	// Workload becomes ready.
	deploy.Status.Replicas = 1
	deploy.Status.ReadyReplicas = 1
	deploy.Status.AvailableReplicas = 1
	if err := k8sClient.Status().Update(context.Background(), deploy); err != nil {
		t.Fatalf("set deployment status: %v", err)
	}

	reconcileCR(t, r, ns, "run")
	if got := getCR(t, ns, "run").Status.Phase; got != PhaseRunning {
		t.Fatalf("phase = %q, want Running once target is ready", got)
	}
	getJob(t, ns, "run-loadgen") // panics via t.Fatalf if absent
}

// TestIntegration_Finalizer_AddedOnFirstReconcile ensures every fresh CR
// gains the finalizer before any other work proceeds, so a delete-during-
// run can drain children deterministically.
func TestIntegration_Finalizer_AddedOnFirstReconcile(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, newScaleValidation(ns, "run", nil))

	reconcileCR(t, r, ns, "run")
	got := getCR(t, ns, "run")
	if !slices.Contains(got.Finalizers, scaleValidationFinalizer) {
		t.Errorf("finalizers = %v, want %s present", got.Finalizers, scaleValidationFinalizer)
	}
}

// TestIntegration_Finalizer_DeleteWithNoChildren handles the case where
// the CR is deleted before any Job spawned (target Deployment was never
// ready, so the reconciler stayed in Pending). The finalizer must drop
// on the first cleanup reconcile.
func TestIntegration_Finalizer_DeleteWithNoChildren(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	// No mustCreateReadyTarget: target Deployment is absent, so the
	// readiness gate holds the CR in Pending without ever spawning a Job.
	mustCreate(t, newScaleValidation(ns, "run", nil))

	reconcileCR(t, r, ns, "run") // adds finalizer + sets Pending
	reconcileCR(t, r, ns, "run") // Pending + no target -> still Pending

	cr := getCR(t, ns, "run")
	if err := k8sClient.Delete(context.Background(), cr); err != nil {
		t.Fatalf("delete CR: %v", err)
	}
	reconcileCR(t, r, ns, "run") // finalize path

	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: "run"}, &v1alpha1.ScaleValidation{}); err == nil {
		t.Error("CR still present after finalize with no children")
	}
}

// TestIntegration_Finalizer_DeleteWithRunningChild deletes a CR whose
// loadgen Job is already running. The finalizer must request Job
// deletion, requeue, then drop and let GC remove the CR.
func TestIntegration_Finalizer_DeleteWithRunningChild(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, newScaleValidation(ns, "run", nil))

	reconcileCR(t, r, ns, "run") // finalizer
	reconcileCR(t, r, ns, "run") // Pending
	reconcileCR(t, r, ns, "run") // Running + Job spawned
	getJob(t, ns, "run-loadgen") // sanity

	cr := getCR(t, ns, "run")
	if err := k8sClient.Delete(context.Background(), cr); err != nil {
		t.Fatalf("delete CR: %v", err)
	}

	// First finalize reconcile: requests Job delete + sets Terminating,
	// requeues for child to clear.
	reconcileCR(t, r, ns, "run")
	got := getCR(t, ns, "run")
	if got.Status.Phase != PhaseTerminating {
		t.Errorf("phase = %q, want Terminating during finalize", got.Status.Phase)
	}

	// Force-clear the Job so the second finalize observes IsNotFound.
	// envtest does not run the GC, so an in-test cleanup stands in.
	var job batchv1.Job
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: "run-loadgen"}, &job); err == nil {
		zero := int64(0)
		if err := k8sClient.Delete(context.Background(), &job, &client.DeleteOptions{
			GracePeriodSeconds: &zero,
		}); err != nil {
			t.Fatalf("force-delete job: %v", err)
		}
	}

	reconcileCR(t, r, ns, "run") // finalizer drops
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: "run"}, &v1alpha1.ScaleValidation{}); err == nil {
		t.Error("CR still present after finalize with running child cleared")
	}
}

// TestIntegration_TLSCABundle_Missing fails the run when spec.target.tls
// references a ConfigMap that does not exist. The reconciler must move the
// CR to Error with a TLSCABundleMissing diagnostic and must not create the
// loadgen Job.
func TestIntegration_TLSCABundle_Missing(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, newScaleValidation(ns, "run", func(cr *v1alpha1.ScaleValidation) {
		cr.Spec.Target.TLS = &v1alpha1.TLSConfig{
			CABundle: &v1alpha1.CABundleSource{
				ConfigMapRef: v1alpha1.ConfigMapKeyRef{Name: "missing-ca", Key: "ca.crt"},
			},
		}
	}))

	reconcileCR(t, r, ns, "run") // -> Pending
	reconcileCR(t, r, ns, "run") // Pending + missing CA -> Error
	got := getCR(t, ns, "run")
	if got.Status.Phase != PhaseError {
		t.Fatalf("phase = %q, want Error after missing CA bundle", got.Status.Phase)
	}
	if len(got.Status.Diagnostics) != 1 || got.Status.Diagnostics[0].Type != "TLSCABundleMissing" {
		t.Errorf("diagnostics = %+v, want one TLSCABundleMissing alert", got.Status.Diagnostics)
	}
	var job batchv1.Job
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: "run-loadgen"}, &job); err == nil {
		t.Error("loadgen Job created despite missing CA bundle")
	}
}

// TestIntegration_TLSCABundle_MissingKey fails the run when the referenced
// ConfigMap exists but does not carry the configured key.
func TestIntegration_TLSCABundle_MissingKey(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "internal-ca", Namespace: ns},
		Data:       map[string]string{"other.crt": "PEM"},
	})
	mustCreate(t, newScaleValidation(ns, "run", func(cr *v1alpha1.ScaleValidation) {
		cr.Spec.Target.TLS = &v1alpha1.TLSConfig{
			CABundle: &v1alpha1.CABundleSource{
				ConfigMapRef: v1alpha1.ConfigMapKeyRef{Name: "internal-ca", Key: "ca.crt"},
			},
		}
	}))

	reconcileCR(t, r, ns, "run")
	reconcileCR(t, r, ns, "run")
	got := getCR(t, ns, "run")
	if got.Status.Phase != PhaseError {
		t.Fatalf("phase = %q, want Error when CA key is missing", got.Status.Phase)
	}
	if len(got.Status.Diagnostics) != 1 || got.Status.Diagnostics[0].Type != "TLSCABundleMissing" {
		t.Errorf("diagnostics = %+v, want one TLSCABundleMissing alert", got.Status.Diagnostics)
	}
}

// TestIntegration_TLSCABundle_Present succeeds when the ConfigMap and key
// exist, and the loadgen Job mounts the bundle.
func TestIntegration_TLSCABundle_Present(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, &corev1.ConfigMap{
		ObjectMeta: metav1.ObjectMeta{Name: "internal-ca", Namespace: ns},
		Data:       map[string]string{"ca.crt": "PEM"},
	})
	mustCreate(t, newScaleValidation(ns, "run", func(cr *v1alpha1.ScaleValidation) {
		cr.Spec.Target.TLS = &v1alpha1.TLSConfig{
			CABundle: &v1alpha1.CABundleSource{
				ConfigMapRef: v1alpha1.ConfigMapKeyRef{Name: "internal-ca", Key: "ca.crt"},
			},
		}
	}))

	reconcileCR(t, r, ns, "run")
	reconcileCR(t, r, ns, "run")
	if got := getCR(t, ns, "run").Status.Phase; got != PhaseRunning {
		t.Fatalf("phase = %q, want Running with valid CA bundle", got)
	}
	job := getJob(t, ns, "run-loadgen")
	args := job.Spec.Template.Spec.Containers[0].Args
	if i := slices.Index(args, "--tls-ca-bundle"); i < 0 {
		t.Errorf("loadgen --tls-ca-bundle flag missing: %v", args)
	}
}

// TestIntegration_Events_HappyPath asserts the happy-path Event sequence
// (LoadgenJobCreated then VerdictPass) fires when a run reaches Succeeded.
func TestIntegration_Events_HappyPath(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	line, err := observer.MarshalReportLine(observer.Report{
		SLAStatus:        observer.VerdictPass,
		TrafficIntegrity: observer.VerdictPass,
		TotalRequests:    100,
	})
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	r.observerLogFn = func(context.Context, *corev1.Pod) ([]byte, error) {
		return []byte(line + "\n"), nil
	}

	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, newScaleValidation(ns, "run", nil))
	reconcileCR(t, r, ns, "run")
	reconcileCR(t, r, ns, "run")
	completeJob(t, ns, "run-loadgen")
	createObserverPod(t, ns, "run")
	reconcileCR(t, r, ns, "run")

	got := drainEventReasons(t, r)
	wantSubset := []string{EventReasonLoadgenJobCreated, EventReasonVerdictPass}
	for _, want := range wantSubset {
		if !slices.Contains(got, want) {
			t.Errorf("event reasons = %v, missing %q", got, want)
		}
	}
}

// TestIntegration_Events_TargetReadyTimeout asserts the timeout path emits
// a Warning TargetReadyTimeout Event with the diagnostic context.
func TestIntegration_Events_TargetReadyTimeout(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	r.targetReadyTimeout = time.Nanosecond
	mustCreate(t, newScaleValidation(ns, "run", nil))

	reconcileCR(t, r, ns, "run")
	reconcileCR(t, r, ns, "run")

	got := drainEventReasons(t, r)
	if !slices.Contains(got, EventReasonTargetReadyTimeout) {
		t.Errorf("event reasons = %v, missing %q", got, EventReasonTargetReadyTimeout)
	}
}

// TestIntegration_Events_VerdictFail asserts the SLA-failed path emits a
// Warning VerdictFail Event embedding the top diagnostic.
func TestIntegration_Events_VerdictFail(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	line, err := observer.MarshalReportLine(observer.Report{
		SLAStatus:        observer.VerdictFail,
		TrafficIntegrity: observer.VerdictPass,
		Diagnostics: []v1alpha1.DiagnosticAlert{
			{Type: "HPAReactSlow", Severity: "Critical", Message: "scale-up exceeded SLA"},
		},
	})
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	r.observerLogFn = func(context.Context, *corev1.Pod) ([]byte, error) {
		return []byte(line + "\n"), nil
	}

	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, newScaleValidation(ns, "run", nil))
	reconcileCR(t, r, ns, "run")
	reconcileCR(t, r, ns, "run")
	completeJob(t, ns, "run-loadgen")
	createObserverPod(t, ns, "run")
	reconcileCR(t, r, ns, "run")

	got := drainEventReasons(t, r)
	if !slices.Contains(got, EventReasonVerdictFail) {
		t.Errorf("event reasons = %v, missing %q", got, EventReasonVerdictFail)
	}
}

// TestIntegration_Events_TLSCABundleMissing asserts the missing-bundle path
// emits a Warning TLSCABundleMissing Event.
func TestIntegration_Events_TLSCABundleMissing(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, newScaleValidation(ns, "run", func(cr *v1alpha1.ScaleValidation) {
		cr.Spec.Target.TLS = &v1alpha1.TLSConfig{
			CABundle: &v1alpha1.CABundleSource{
				ConfigMapRef: v1alpha1.ConfigMapKeyRef{Name: "missing-ca", Key: "ca.crt"},
			},
		}
	}))

	reconcileCR(t, r, ns, "run")
	reconcileCR(t, r, ns, "run")

	got := drainEventReasons(t, r)
	if !slices.Contains(got, EventReasonTLSCABundleMissing) {
		t.Errorf("event reasons = %v, missing %q", got, EventReasonTLSCABundleMissing)
	}
}

// TestIntegration_TargetNotReady_TimesOut drives the timeout branch via
// the test-only targetReadyTimeout override: a 1ns timeout means the very
// first gate check (target missing) is over the limit, so the CR is
// failed with a TargetNotReady diagnostic instead of looping forever.
func TestIntegration_TargetNotReady_TimesOut(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	r.targetReadyTimeout = time.Nanosecond
	mustCreate(t, newScaleValidation(ns, "run", nil))

	reconcileCR(t, r, ns, "run") // empty -> Pending
	reconcileCR(t, r, ns, "run") // Pending + no target + timeout -> Error
	got := getCR(t, ns, "run")
	if got.Status.Phase != PhaseError {
		t.Fatalf("phase = %q, want Error after readiness timeout", got.Status.Phase)
	}
	if len(got.Status.Diagnostics) != 1 || got.Status.Diagnostics[0].Type != "TargetNotReady" {
		t.Errorf("diagnostics = %+v, want one TargetNotReady alert", got.Status.Diagnostics)
	}
	if got.Status.Diagnostics[0].Severity != "Critical" {
		t.Errorf("severity = %q, want Critical", got.Status.Diagnostics[0].Severity)
	}
}

// createHealthyTargetPod creates a pod labeled like the target Deployment's
// selector and patches it Running + Ready, so chaos.HealthyPods counts it
// as an eligible disruption victim. envtest runs no kubelet, so pod status
// is whatever the test writes.
func createHealthyTargetPod(t *testing.T, ns, name, app string) {
	t.Helper()
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns, Labels: map[string]string{"app": app}},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "web:1"}}},
	}
	mustCreate(t, pod)
	pod.Status.Phase = corev1.PodRunning
	pod.Status.Conditions = []corev1.PodCondition{{Type: corev1.PodReady, Status: corev1.ConditionTrue}}
	if err := k8sClient.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("set pod status: %v", err)
	}
}

// countAliveTargetPods returns how many app-labeled pods exist without a
// deletionTimestamp. Unscheduled pods (no nodeName) are deleted immediately
// by the apiserver rather than parked in Terminating, so "alive" is the
// deletion signal that holds under both semantics.
func countAliveTargetPods(t *testing.T, ns, app string) int {
	t.Helper()
	var pods corev1.PodList
	if err := k8sClient.List(context.Background(), &pods,
		client.InNamespace(ns), client.MatchingLabels{"app": app}); err != nil {
		t.Fatalf("list target pods: %v", err)
	}
	alive := 0
	for _, p := range pods.Items {
		if p.DeletionTimestamp == nil {
			alive++
		}
	}
	return alive
}

// TestIntegration_Disruption_DeletesVictim drives the chaos-injection path:
// a Running CR with spec.disruption and two healthy target pods deletes
// exactly one victim, records the DisruptionInjected condition, and never
// injects a second time on later reconciles.
func TestIntegration_Disruption_DeletesVictim(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	mustCreateReadyTarget(t, ns, "app")
	createHealthyTargetPod(t, ns, "app-0", "app")
	createHealthyTargetPod(t, ns, "app-1", "app")
	mustCreate(t, newScaleValidation(ns, "chaos", func(cr *v1alpha1.ScaleValidation) {
		cr.Spec.Disruption = &v1alpha1.DisruptionConfig{
			InjectPodDeletion:   true,
			MinReplicasForChaos: 2,
		}
	}))

	reconcileCR(t, r, ns, "chaos") // empty -> Pending
	reconcileCR(t, r, ns, "chaos") // Pending -> Running (job spawned)
	reconcileCR(t, r, ns, "chaos") // Running + zero trigger delay -> inject

	if got := countAliveTargetPods(t, ns, "app"); got != 1 {
		t.Fatalf("alive target pods = %d, want 1 (exactly one victim deleted)", got)
	}
	cond := meta.FindStatusCondition(getCR(t, ns, "chaos").Status.Conditions, ConditionDisruptionInjected)
	if cond == nil || cond.Status != metav1.ConditionTrue || cond.Reason != DisruptionReasonPodDeleted {
		t.Fatalf("DisruptionInjected condition = %+v, want True/PodDeleted", cond)
	}
	if got := drainEventReasons(t, r); !slices.Contains(got, EventReasonChaosInjected) {
		t.Errorf("event reasons = %v, missing %q", got, EventReasonChaosInjected)
	}

	// The condition is the re-injection guard: another reconcile must not
	// touch the surviving pod.
	reconcileCR(t, r, ns, "chaos")
	if got := countAliveTargetPods(t, ns, "app"); got != 1 {
		t.Fatalf("alive target pods after re-reconcile = %d, want still 1", got)
	}
}

// TestIntegration_Disruption_SkippedBelowMinReplicas asserts the safety
// gate: one healthy replica with minReplicasForChaos=2 records a skip and
// deletes nothing.
func TestIntegration_Disruption_SkippedBelowMinReplicas(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	mustCreateReadyTarget(t, ns, "app")
	createHealthyTargetPod(t, ns, "app-0", "app")
	mustCreate(t, newScaleValidation(ns, "chaos", func(cr *v1alpha1.ScaleValidation) {
		cr.Spec.Disruption = &v1alpha1.DisruptionConfig{
			InjectPodDeletion:   true,
			MinReplicasForChaos: 2,
		}
	}))

	reconcileCR(t, r, ns, "chaos")
	reconcileCR(t, r, ns, "chaos")
	reconcileCR(t, r, ns, "chaos")

	if got := countAliveTargetPods(t, ns, "app"); got != 1 {
		t.Fatalf("alive target pods = %d, want 1 (gated, nothing deleted)", got)
	}
	cond := meta.FindStatusCondition(getCR(t, ns, "chaos").Status.Conditions, ConditionDisruptionInjected)
	if cond == nil || cond.Status != metav1.ConditionFalse || cond.Reason != DisruptionReasonSkipped {
		t.Fatalf("DisruptionInjected condition = %+v, want False/Skipped", cond)
	}
	if got := drainEventReasons(t, r); !slices.Contains(got, EventReasonChaosSkipped) {
		t.Errorf("event reasons = %v, missing %q", got, EventReasonChaosSkipped)
	}
}

// TestIntegration_Disruption_TriggerDelayRequeues asserts that before the
// trigger point the reconciler requeues for the remaining delay instead of
// injecting early.
func TestIntegration_Disruption_TriggerDelayRequeues(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	mustCreateReadyTarget(t, ns, "app")
	createHealthyTargetPod(t, ns, "app-0", "app")
	createHealthyTargetPod(t, ns, "app-1", "app")
	mustCreate(t, newScaleValidation(ns, "chaos", func(cr *v1alpha1.ScaleValidation) {
		cr.Spec.Disruption = &v1alpha1.DisruptionConfig{
			InjectPodDeletion:   true,
			MinReplicasForChaos: 2,
			TriggerDelay:        &metav1.Duration{Duration: time.Hour},
		}
	}))

	reconcileCR(t, r, ns, "chaos")
	reconcileCR(t, r, ns, "chaos")
	res, err := r.Reconcile(context.Background(), ctrl.Request{
		NamespacedName: types.NamespacedName{Namespace: ns, Name: "chaos"},
	})
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if res.RequeueAfter <= 0 || res.RequeueAfter > time.Hour {
		t.Fatalf("RequeueAfter = %s, want in (0, 1h]", res.RequeueAfter)
	}
	if got := countAliveTargetPods(t, ns, "app"); got != 2 {
		t.Fatalf("alive target pods = %d, want 2 before trigger point", got)
	}
	if cond := meta.FindStatusCondition(getCR(t, ns, "chaos").Status.Conditions, ConditionDisruptionInjected); cond != nil {
		t.Fatalf("DisruptionInjected condition = %+v, want absent before trigger point", cond)
	}
}
