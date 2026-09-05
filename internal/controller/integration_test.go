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
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"k8s.io/client-go/tools/record"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1 "github.com/ethan-kane-ops/scale-sentry/api/v1beta1"
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

func newScaleValidation(ns, name string, mod func(*v1beta1.ScaleValidation)) *v1beta1.ScaleValidation {
	cr := &v1beta1.ScaleValidation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: ns},
		Spec: v1beta1.ScaleValidationSpec{
			TargetRef: v1beta1.CrossVersionObjectReference{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "app",
			},
			SLA:    metav1.Duration{Duration: time.Minute},
			Target: v1beta1.TargetConfig{Mode: "ServiceDefault", Port: 8080, NetworkPath: "ClusterIP"},
			Load:   v1beta1.LoadConfig{BaseRPS: 100},
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
// gives it one ready Pod. The reconciler's readiness gate holds the loadgen
// Job back until at least one pod behind the target's scale-subresource
// selector is Ready, and envtest runs no kubelet or workload controllers,
// so that pod has to be made by hand.
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
	createHealthyTargetPod(t, ns, name+"-pod", name)
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

func getCR(t *testing.T, ns, name string) *v1beta1.ScaleValidation {
	t.Helper()
	var cr v1beta1.ScaleValidation
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
	// A scheduled CR runs more than once. In production the previous
	// run's pod is garbage-collected with its Job; envtest runs no GC, so
	// the create is tolerant and the status is refreshed instead.
	if err := k8sClient.Create(context.Background(), pod); err != nil {
		if !apierrors.IsAlreadyExists(err) {
			t.Fatalf("create observer pod: %v", err)
		}
		if err := k8sClient.Get(context.Background(),
			types.NamespacedName{Namespace: ns, Name: pod.Name}, pod); err != nil {
			t.Fatalf("get existing observer pod: %v", err)
		}
	}
	pod.Status.InitContainerStatuses = []corev1.ContainerStatus{{
		Name:  observerContainerName,
		State: corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{ExitCode: 0}},
	}}
	if err := k8sClient.Status().Update(context.Background(), pod); err != nil {
		t.Fatalf("set pod status: %v", err)
	}
}

// assertFinished checks that a terminal CR carries Finished=True with the
// expected reason. Every terminal path must set it: a CI gate blocking on
// `kubectl wait --for=condition=Finished` hangs until its own timeout
// against any path that forgets, which is the failure ENG-149 removes.
func assertFinished(t *testing.T, cr *v1beta1.ScaleValidation, wantReason string) {
	t.Helper()
	cond := meta.FindStatusCondition(cr.Status.Conditions, ConditionFinished)
	if cond == nil {
		t.Fatalf("no %s condition on a terminal CR (phase=%s)", ConditionFinished, cr.Status.Phase)
	}
	if cond.Status != metav1.ConditionTrue {
		t.Errorf("%s = %s, want True", ConditionFinished, cond.Status)
	}
	if cond.Reason != wantReason {
		t.Errorf("%s reason = %s, want %s", ConditionFinished, cond.Reason, wantReason)
	}
	if cond.ObservedGeneration != cr.Generation {
		t.Errorf("observedGeneration = %d, want %d", cond.ObservedGeneration, cr.Generation)
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
	cr = getCR(t, ns, "run")
	if cr.Status.Phase != PhaseError {
		t.Fatalf("phase = %q, want Error after the job vanished", cr.Status.Phase)
	}
	assertFinished(t, cr, FinishedReasonLoadgenJobVanished)
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
	failed := getCR(t, ns, "run")
	if failed.Status.Phase != PhaseError {
		t.Fatalf("phase = %q, want Error after job failed", failed.Status.Phase)
	}
	assertFinished(t, failed, FinishedReasonLoadgenJobFailed)
}

func TestIntegration_FinishRunSucceeded(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	line, err := observer.MarshalReportLine(observer.Report{
		SLAStatus:        observer.VerdictPass,
		TrafficIntegrity: observer.VerdictPass,
		TotalRequests:    5000,
		FailedRequests:   3,
		Diagnostics: []v1beta1.DiagnosticAlert{
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
	assertFinished(t, got, FinishedReasonSucceeded)
	if got.Status.TotalRequests != 5000 || got.Status.FailedRequests != 3 {
		t.Errorf("metrics = %d/%d, want 5000/3", got.Status.TotalRequests, got.Status.FailedRequests)
	}
	if got.Status.SLAStatus != "Pass" || got.Status.TrafficIntegrity != "Pass" {
		t.Errorf("verdicts = %s/%s, want Pass/Pass", got.Status.SLAStatus, got.Status.TrafficIntegrity)
	}
	if len(got.Status.Diagnostics) != 1 || got.Status.Diagnostics[0].Type != "CPUThrottling" {
		t.Errorf("diagnostics = %+v, want one CPUThrottling alert", got.Status.Diagnostics)
	}
	if len(got.Status.History) != 1 || got.Status.History[0].Phase != PhaseSucceeded ||
		got.Status.History[0].SLAStatus != "Pass" {
		t.Errorf("history = %+v, want one Succeeded/Pass entry", got.Status.History)
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
	assertFinished(t, getCR(t, ns, "run"), FinishedReasonVerdictFailed)
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

	var sv v1beta1.ScaleValidation
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
	createHealthyTargetPod(t, ns, "app-pod", "app")
	mustCreate(t, newScaleValidation(ns, "run", func(cr *v1beta1.ScaleValidation) {
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
		t.Fatalf("phase = %q, want Pending while no target pod is ready", got)
	}

	// Workload becomes ready: a pod behind the Deployment's selector
	// reports Ready.
	createHealthyTargetPod(t, ns, "app-pod", "app")

	reconcileCR(t, r, ns, "run")
	if got := getCR(t, ns, "run").Status.Phase; got != PhaseRunning {
		t.Fatalf("phase = %q, want Running once target is ready", got)
	}
	getJob(t, ns, "run-loadgen") // panics via t.Fatalf if absent
}

// TestIntegration_StatefulSetTarget is the ENG-148 regression test.
// spec.targetRef.kind used to be ignored: a StatefulSet target was probed
// as a Deployment of the same name, so the run stalled and eventually
// reported TargetNotReady against a workload that was perfectly healthy.
// The target is now resolved through its scale subresource, which every
// scalable kind serves.
func TestIntegration_StatefulSetTarget(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()

	labels := map[string]string{"app": "sts"}
	sts := &appsv1.StatefulSet{
		ObjectMeta: metav1.ObjectMeta{Name: "sts", Namespace: ns},
		Spec: appsv1.StatefulSetSpec{
			ServiceName: "sts",
			Selector:    &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web", Image: "web:1"}}},
			},
		},
	}
	mustCreate(t, sts)
	createHealthyTargetPod(t, ns, "sts-0", "sts")

	mustCreate(t, newScaleValidation(ns, "run", func(cr *v1beta1.ScaleValidation) {
		cr.Spec.TargetRef.Kind = "StatefulSet"
		cr.Spec.TargetRef.Name = "sts"
	}))

	reconcileCR(t, r, ns, "run")
	reconcileCR(t, r, ns, "run")

	if got := getCR(t, ns, "run").Status.Phase; got != PhaseRunning {
		t.Fatalf("phase = %q, want Running for a ready StatefulSet target", got)
	}
	job := getJob(t, ns, "run-loadgen")
	args := job.Spec.Template.Spec.InitContainers[0].Args
	for flag, want := range map[string]string{
		"--target-kind":     "StatefulSet",
		"--target-resource": "statefulsets",
		"--target-group":    "apps",
	} {
		if got := flagValue(args, flag); got != want {
			t.Errorf("observer %s = %q, want %q (args %v)", flag, got, want, args)
		}
	}
}

// flagValue returns the value following flag in args, or "" if absent.
func flagValue(args []string, flag string) string {
	for i, a := range args {
		if a == flag && i+1 < len(args) {
			return args[i+1]
		}
	}
	return ""
}

// TestIntegration_UnknownTargetKind_FailsFast covers the other half of
// ENG-148: a kind the cluster does not serve is a spec error, so the run
// fails immediately with a diagnostic naming the kind instead of silently
// waiting out the readiness window against a Deployment lookup.
func TestIntegration_UnknownTargetKind_FailsFast(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	mustCreate(t, newScaleValidation(ns, "run", func(cr *v1beta1.ScaleValidation) {
		cr.Spec.TargetRef.APIVersion = "argoproj.io/v1beta1"
		cr.Spec.TargetRef.Kind = "Rollout"
		cr.Spec.TargetRef.Name = "rollout"
	}))

	reconcileCR(t, r, ns, "run")
	reconcileCR(t, r, ns, "run")

	cr := getCR(t, ns, "run")
	if cr.Status.Phase != PhaseError {
		t.Fatalf("phase = %q, want Error for an unresolvable targetRef kind", cr.Status.Phase)
	}
	assertFinished(t, cr, FinishedReasonTargetUnsupported)
	var found *v1beta1.DiagnosticAlert
	for i := range cr.Status.Diagnostics {
		if cr.Status.Diagnostics[i].Type == "TargetUnsupported" {
			found = &cr.Status.Diagnostics[i]
		}
	}
	if found == nil {
		t.Fatalf("no TargetUnsupported diagnostic, got %+v", cr.Status.Diagnostics)
	}
	if !strings.Contains(found.Message, "Rollout") {
		t.Errorf("diagnostic should name the unresolvable kind, got %q", found.Message)
	}
	if reasons := drainEventReasons(t, r); !slices.Contains(reasons, EventReasonTargetUnresolvable) {
		t.Errorf("Events = %v, want %s", reasons, EventReasonTargetUnresolvable)
	}
}

// TestIntegration_FinishedCondition_AbsentUntilTerminal is the CI-gate
// contract ENG-149 exists for. `kubectl wait --for=condition=Finished`
// must block for as long as the run is in flight and return the moment it
// ends, whichever way it ends. If the condition appeared early a gate
// would pass a run that had not finished; if it never appeared on a
// failure path the gate would hang until its own --timeout, which is the
// behaviour this replaces.
func TestIntegration_FinishedCondition_AbsentUntilTerminal(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	line, err := observer.MarshalReportLine(observer.Report{
		SLAStatus:        observer.VerdictFail,
		TrafficIntegrity: observer.VerdictPass,
		TotalRequests:    100,
		FailedRequests:   0,
	})
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	r.observerLogFn = func(context.Context, *corev1.Pod) ([]byte, error) {
		return []byte(line + "\n"), nil
	}

	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, newScaleValidation(ns, "run", nil))

	reconcileCR(t, r, ns, "run") // -> Pending
	if cond := meta.FindStatusCondition(getCR(t, ns, "run").Status.Conditions, ConditionFinished); cond != nil {
		t.Fatalf("%s set while Pending: %+v", ConditionFinished, cond)
	}

	reconcileCR(t, r, ns, "run") // -> Running
	running := getCR(t, ns, "run")
	if running.Status.Phase != PhaseRunning {
		t.Fatalf("phase = %q, want Running", running.Status.Phase)
	}
	if cond := meta.FindStatusCondition(running.Status.Conditions, ConditionFinished); cond != nil {
		t.Fatalf("%s set while Running: %+v", ConditionFinished, cond)
	}

	completeJob(t, ns, "run-loadgen")
	createObserverPod(t, ns, "run")
	reconcileCR(t, r, ns, "run") // -> Failed verdict

	done := getCR(t, ns, "run")
	if done.Status.Phase != PhaseFailed {
		t.Fatalf("phase = %q, want Failed", done.Status.Phase)
	}
	// The whole point: a losing verdict still sets Finished=True, so the
	// gate unblocks and reads the phase rather than timing out.
	assertFinished(t, done, FinishedReasonVerdictFailed)
	if cond := meta.FindStatusCondition(done.Status.Conditions, ConditionFinished); !strings.Contains(cond.Message, "SLA=Fail") {
		t.Errorf("condition message = %q, want it to carry the verdict", cond.Message)
	}
}

// driveOneRun reconciles until exactly one more verdict lands in
// status.history, completing the loadgen Job whenever one appears. A
// scheduled re-run needs a variable number of passes (tear down the
// finished Job, reset, respawn), so the loop is bounded rather than
// counted. History growing by one is the unambiguous "a run completed"
// signal, since a scheduled CR is already in a terminal phase when its
// next run begins.
func driveOneRun(t *testing.T, r *ScaleValidationReconciler, ns, name string) *v1beta1.ScaleValidation {
	t.Helper()
	start := len(getCR(t, ns, name).Status.History)
	for range 15 {
		cr := getCR(t, ns, name)
		if len(cr.Status.History) > start {
			return cr
		}
		if cr.Status.Phase == PhaseRunning {
			completeJob(t, ns, name+"-loadgen")
			createObserverPod(t, ns, name)
		}
		reconcileCR(t, r, ns, name)
	}
	t.Fatalf("run did not produce a verdict within 15 reconciles (history stuck at %d)", start)
	return nil
}

// advanceClock points the reconciler at a movable clock and returns a
// function that pushes it forward. Schedule arithmetic is the only thing
// that reads it, so a scheduled run can be made due without sleeping
// through a real cron interval (robfig/cron floors @every at one second).
func advanceClock(r *ScaleValidationReconciler) func(time.Duration) {
	var offset time.Duration
	r.nowFn = func() time.Time { return time.Now().Add(offset) }
	return func(d time.Duration) { offset += d }
}

// passingReconciler returns a reconciler whose observer log always reports
// a clean run, so scheduling tests are not entangled with verdict logic.
func passingReconciler(t *testing.T) *ScaleValidationReconciler {
	t.Helper()
	r := newReconciler()
	line, err := observer.MarshalReportLine(observer.Report{
		SLAStatus:        observer.VerdictPass,
		TrafficIntegrity: observer.VerdictPass,
		TotalRequests:    1000,
	})
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	r.observerLogFn = func(context.Context, *corev1.Pod) ([]byte, error) {
		return []byte(line + "\n"), nil
	}
	return r
}

// TestIntegration_Schedule_RerunsAndAccumulatesHistory is the ENG-150
// headline. Before it, a terminal CR was never reconciled again, so
// status.history could only ever hold the single entry its one run wrote,
// and the documented "last ten terminal verdicts" was unreachable.
func TestIntegration_Schedule_RerunsAndAccumulatesHistory(t *testing.T) {
	ns := newTestNamespace(t)
	r := passingReconciler(t)
	advance := advanceClock(r)
	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, newScaleValidation(ns, "run", func(cr *v1beta1.ScaleValidation) {
		cr.Spec.Schedule = "@every 1s"
	}))

	for run := 1; run <= 3; run++ {
		cr := driveOneRun(t, r, ns, "run")
		advance(time.Minute) // the next run becomes due
		if len(cr.Status.History) != run {
			t.Fatalf("after run %d: history = %d entries, want %d", run, len(cr.Status.History), run)
		}
		if cr.Status.Phase != PhaseSucceeded {
			t.Fatalf("after run %d: phase = %q, want Succeeded", run, cr.Status.Phase)
		}
		assertFinished(t, cr, FinishedReasonSucceeded)
	}
}

// TestIntegration_Schedule_ResetsRunScopedStatus checks that a re-run
// reports its own results rather than inheriting the previous run's, and
// that the chaos guard is cleared so disruption still fires.
func TestIntegration_Schedule_ResetsRunScopedStatus(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	failing, err := observer.MarshalReportLine(observer.Report{
		SLAStatus:        observer.VerdictFail,
		TrafficIntegrity: observer.VerdictPass,
		TotalRequests:    10,
		FailedRequests:   9,
		Diagnostics:      []v1beta1.DiagnosticAlert{{Type: "CPUThrottling", Severity: "Warning", Message: "throttled"}},
	})
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	passing, err := observer.MarshalReportLine(observer.Report{
		SLAStatus:        observer.VerdictPass,
		TrafficIntegrity: observer.VerdictPass,
		TotalRequests:    1000,
	})
	if err != nil {
		t.Fatalf("marshal report: %v", err)
	}
	report := failing
	r.observerLogFn = func(context.Context, *corev1.Pod) ([]byte, error) {
		return []byte(report + "\n"), nil
	}
	advance := advanceClock(r)

	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, newScaleValidation(ns, "run", func(cr *v1beta1.ScaleValidation) {
		cr.Spec.Schedule = "@every 1s"
	}))

	first := driveOneRun(t, r, ns, "run")
	if first.Status.Phase != PhaseFailed || len(first.Status.Diagnostics) != 1 {
		t.Fatalf("first run: phase=%q diagnostics=%d, want Failed with 1", first.Status.Phase, len(first.Status.Diagnostics))
	}

	report = passing
	advance(time.Minute)
	second := driveOneRun(t, r, ns, "run")
	if second.Status.Phase != PhaseSucceeded {
		t.Fatalf("second run: phase = %q, want Succeeded", second.Status.Phase)
	}
	if len(second.Status.Diagnostics) != 0 {
		t.Errorf("second run inherited the first run's diagnostics: %+v", second.Status.Diagnostics)
	}
	if second.Status.FailedRequests != 0 || second.Status.SLAStatus != "Pass" {
		t.Errorf("second run kept stale verdict fields: failed=%d sla=%q",
			second.Status.FailedRequests, second.Status.SLAStatus)
	}
	if len(second.Status.History) != 2 ||
		second.Status.History[0].Phase != PhaseSucceeded ||
		second.Status.History[1].Phase != PhaseFailed {
		t.Errorf("history = %+v, want newest-first Succeeded then Failed", second.Status.History)
	}
}

// TestIntegration_Schedule_Suspend asserts suspend stops future runs and
// withdraws the advertised next run, without disturbing the last verdict.
func TestIntegration_Schedule_Suspend(t *testing.T) {
	ns := newTestNamespace(t)
	r := passingReconciler(t)
	advance := advanceClock(r)
	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, newScaleValidation(ns, "run", func(cr *v1beta1.ScaleValidation) {
		cr.Spec.Schedule = "@every 1s"
	}))

	first := driveOneRun(t, r, ns, "run")
	if len(first.Status.History) != 1 {
		t.Fatalf("history = %d, want 1", len(first.Status.History))
	}

	cr := getCR(t, ns, "run")
	cr.Spec.Suspend = true
	if err := k8sClient.Update(context.Background(), cr); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	// Push past the next due time: if suspend did nothing, a run would
	// start here, so the assertions below are meaningful.
	advance(time.Minute)

	for range 5 {
		reconcileCR(t, r, ns, "run")
	}

	got := getCR(t, ns, "run")
	if len(got.Status.History) != 1 {
		t.Errorf("history = %d entries while suspended, want 1", len(got.Status.History))
	}
	if got.Status.Phase != PhaseSucceeded {
		t.Errorf("phase = %q, want the last verdict preserved", got.Status.Phase)
	}
	if got.Status.NextRunTime != nil {
		t.Errorf("nextRunTime = %v while suspended, want nil", got.Status.NextRunTime)
	}
}

// TestIntegration_Schedule_PublishesNextRunTime covers the far-future
// case: the CR parks in its terminal phase and advertises when it will run
// again, which is what the Next Run printer column reads.
func TestIntegration_Schedule_PublishesNextRunTime(t *testing.T) {
	ns := newTestNamespace(t)
	r := passingReconciler(t)
	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, newScaleValidation(ns, "run", func(cr *v1beta1.ScaleValidation) {
		cr.Spec.Schedule = "0 2 * * *" // daily at 02:00
	}))

	first := driveOneRun(t, r, ns, "run")
	if first.Status.Phase != PhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded", first.Status.Phase)
	}

	reconcileCR(t, r, ns, "run")
	got := getCR(t, ns, "run")
	if got.Status.NextRunTime == nil {
		t.Fatal("nextRunTime not published for a scheduled validation")
	}
	if !got.Status.NextRunTime.After(time.Now()) {
		t.Errorf("nextRunTime = %v, want a future time", got.Status.NextRunTime)
	}
	if len(got.Status.History) != 1 {
		t.Errorf("history = %d, want 1 (the next run is not due yet)", len(got.Status.History))
	}
}

// TestIntegration_Schedule_InvalidRejectedUpFront asserts a bad cron
// expression fails on the first reconcile, before a run is spawned,
// instead of running once and then stalling silently.
func TestIntegration_Schedule_InvalidRejectedUpFront(t *testing.T) {
	ns := newTestNamespace(t)
	r := newReconciler()
	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, newScaleValidation(ns, "run", func(cr *v1beta1.ScaleValidation) {
		cr.Spec.Schedule = "every tuesday please"
	}))

	reconcileCR(t, r, ns, "run")

	cr := getCR(t, ns, "run")
	if cr.Status.Phase != PhaseError {
		t.Fatalf("phase = %q, want Error for an invalid schedule", cr.Status.Phase)
	}
	assertFinished(t, cr, FinishedReasonScheduleInvalid)
	if len(cr.Status.Diagnostics) != 1 || cr.Status.Diagnostics[0].Type != "ScheduleInvalid" {
		t.Errorf("diagnostics = %+v, want one ScheduleInvalid alert", cr.Status.Diagnostics)
	}
	var job batchv1.Job
	if err := k8sClient.Get(context.Background(),
		types.NamespacedName{Namespace: ns, Name: "run-loadgen"}, &job); err == nil {
		t.Error("loadgen Job spawned despite an invalid schedule")
	}
	if reasons := drainEventReasons(t, r); !slices.Contains(reasons, EventReasonScheduleInvalid) {
		t.Errorf("events = %v, want %s", reasons, EventReasonScheduleInvalid)
	}
}

// TestIntegration_Schedule_BecomesInvalidAfterFirstRun covers a schedule
// edited to something unparseable after the CR has already run. Before
// ENG-153 the CR silently stopped rescheduling while advertising a stale
// verdict; now the edit is noticed and the CR says why it stopped.
func TestIntegration_Schedule_BecomesInvalidAfterFirstRun(t *testing.T) {
	ns := newTestNamespace(t)
	r := passingReconciler(t)
	advance := advanceClock(r)
	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, newScaleValidation(ns, "run", func(cr *v1beta1.ScaleValidation) {
		cr.Spec.Schedule = "@every 1s"
	}))

	first := driveOneRun(t, r, ns, "run")
	if len(first.Status.History) != 1 {
		t.Fatalf("history = %d, want 1", len(first.Status.History))
	}

	cr := getCR(t, ns, "run")
	cr.Spec.Schedule = "wednesdays maybe"
	if err := k8sClient.Update(context.Background(), cr); err != nil {
		t.Fatalf("break the schedule: %v", err)
	}
	advance(time.Minute)

	for range 5 {
		reconcileCR(t, r, ns, "run")
	}

	got := getCR(t, ns, "run")
	if got.Status.Phase != PhaseError {
		t.Fatalf("phase = %q, want Error naming the broken schedule", got.Status.Phase)
	}
	assertFinished(t, got, FinishedReasonScheduleInvalid)
	if len(got.Status.History) != 1 {
		t.Errorf("history = %d entries, want 1 (an unparseable schedule must not re-run)", len(got.Status.History))
	}
	if len(got.Status.Diagnostics) != 1 || got.Status.Diagnostics[0].Type != "ScheduleInvalid" {
		t.Errorf("diagnostics = %+v, want exactly one ScheduleInvalid alert", got.Status.Diagnostics)
	}
}

// TestIntegration_SpecChange_RecoversABrokenSchedule is the other half:
// the CR parked in Error above must come back by editing the schedule to
// something valid, without being deleted and recreated.
func TestIntegration_SpecChange_RecoversABrokenSchedule(t *testing.T) {
	ns := newTestNamespace(t)
	r := passingReconciler(t)
	advance := advanceClock(r)
	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, newScaleValidation(ns, "run", func(cr *v1beta1.ScaleValidation) {
		cr.Spec.Schedule = "not a schedule"
	}))

	reconcileCR(t, r, ns, "run")
	if got := getCR(t, ns, "run"); got.Status.Phase != PhaseError {
		t.Fatalf("phase = %q, want Error for an invalid schedule", got.Status.Phase)
	}

	cr := getCR(t, ns, "run")
	cr.Spec.Schedule = "@every 1s"
	if err := k8sClient.Update(context.Background(), cr); err != nil {
		t.Fatalf("fix the schedule: %v", err)
	}
	advance(time.Minute)

	fixed := driveOneRun(t, r, ns, "run")
	if fixed.Status.Phase != PhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded once the schedule is valid", fixed.Status.Phase)
	}
	if len(fixed.Status.Diagnostics) != 0 {
		t.Errorf("diagnostics = %+v, want the stale ScheduleInvalid cleared", fixed.Status.Diagnostics)
	}
}

// TestIntegration_SpecChange_RerunsAOneShot covers the plain case: editing
// a terminal one-shot CR runs it again, where it used to be ignored with
// no error and no Event.
func TestIntegration_SpecChange_RerunsAOneShot(t *testing.T) {
	ns := newTestNamespace(t)
	r := passingReconciler(t)
	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, newScaleValidation(ns, "run", nil))

	first := driveOneRun(t, r, ns, "run")
	if first.Status.ObservedGeneration != first.Generation {
		t.Fatalf("observedGeneration = %d, want %d", first.Status.ObservedGeneration, first.Generation)
	}

	cr := getCR(t, ns, "run")
	cr.Spec.Load.BaseRPS = 250
	if err := k8sClient.Update(context.Background(), cr); err != nil {
		t.Fatalf("edit spec: %v", err)
	}

	second := driveOneRun(t, r, ns, "run")
	if len(second.Status.History) != 2 {
		t.Errorf("history = %d entries, want 2 (the edit should have re-run it)", len(second.Status.History))
	}
	if second.Status.ObservedGeneration != second.Generation {
		t.Errorf("observedGeneration = %d, want %d", second.Status.ObservedGeneration, second.Generation)
	}
	if reasons := drainEventReasons(t, r); !slices.Contains(reasons, EventReasonSpecChanged) {
		t.Errorf("events = %v, want %s", reasons, EventReasonSpecChanged)
	}
}

// TestIntegration_SpecChange_StatusWritesDoNotLoop guards the obvious
// hazard: status updates must not bump metadata.generation, or a terminal
// CR would restart itself forever.
func TestIntegration_SpecChange_StatusWritesDoNotLoop(t *testing.T) {
	ns := newTestNamespace(t)
	r := passingReconciler(t)
	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, newScaleValidation(ns, "run", nil))

	first := driveOneRun(t, r, ns, "run")
	gen := first.Generation

	for range 10 {
		reconcileCR(t, r, ns, "run")
	}

	got := getCR(t, ns, "run")
	if len(got.Status.History) != 1 {
		t.Errorf("history = %d entries after 10 reconciles, want 1 (no self-restart loop)", len(got.Status.History))
	}
	if got.Generation != gen {
		t.Errorf("generation moved from %d to %d without a spec edit", gen, got.Generation)
	}
}

// TestIntegration_Suspend_OutranksASpecEdit pins the ordering ENG-153
// nearly got wrong: setting spec.suspend is itself a spec edit, so a naive
// drift check makes the act of suspending start a run.
func TestIntegration_Suspend_OutranksASpecEdit(t *testing.T) {
	ns := newTestNamespace(t)
	r := passingReconciler(t)
	advance := advanceClock(r)
	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, newScaleValidation(ns, "run", func(cr *v1beta1.ScaleValidation) {
		cr.Spec.Schedule = "@every 1s"
	}))

	driveOneRun(t, r, ns, "run")

	cr := getCR(t, ns, "run")
	cr.Spec.Suspend = true
	cr.Spec.Load.BaseRPS = 250 // suspend must win even alongside a real edit
	if err := k8sClient.Update(context.Background(), cr); err != nil {
		t.Fatalf("suspend: %v", err)
	}
	advance(time.Minute)

	for range 5 {
		reconcileCR(t, r, ns, "run")
	}

	got := getCR(t, ns, "run")
	if len(got.Status.History) != 1 {
		t.Errorf("history = %d entries, want 1 (suspend must outrank the edit)", len(got.Status.History))
	}
	if got.Status.ObservedGeneration != got.Generation {
		t.Errorf("observedGeneration = %d, want %d so unsuspending is not a replayed drift",
			got.Status.ObservedGeneration, got.Generation)
	}
}

// TestIntegration_OneShot_StaysTerminal is the regression guard for every
// CR that predates scheduling: with no spec.schedule, a terminal CR must
// stay exactly where it is, however many times it is reconciled.
func TestIntegration_OneShot_StaysTerminal(t *testing.T) {
	ns := newTestNamespace(t)
	r := passingReconciler(t)
	mustCreateReadyTarget(t, ns, "app")
	mustCreate(t, newScaleValidation(ns, "run", nil))

	first := driveOneRun(t, r, ns, "run")
	if first.Status.Phase != PhaseSucceeded {
		t.Fatalf("phase = %q, want Succeeded", first.Status.Phase)
	}

	for range 5 {
		reconcileCR(t, r, ns, "run")
	}

	got := getCR(t, ns, "run")
	if len(got.Status.History) != 1 {
		t.Errorf("history = %d entries, want 1 (a one-shot run must not repeat)", len(got.Status.History))
	}
	if got.Status.NextRunTime != nil {
		t.Errorf("nextRunTime = %v, want nil without a schedule", got.Status.NextRunTime)
	}
	assertFinished(t, got, FinishedReasonSucceeded)
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
		types.NamespacedName{Namespace: ns, Name: "run"}, &v1beta1.ScaleValidation{}); err == nil {
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
		types.NamespacedName{Namespace: ns, Name: "run"}, &v1beta1.ScaleValidation{}); err == nil {
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
	mustCreate(t, newScaleValidation(ns, "run", func(cr *v1beta1.ScaleValidation) {
		cr.Spec.Target.TLS = &v1beta1.TLSConfig{
			CABundle: &v1beta1.CABundleSource{
				ConfigMapRef: v1beta1.ConfigMapKeyRef{Name: "missing-ca", Key: "ca.crt"},
			},
		}
	}))

	reconcileCR(t, r, ns, "run") // -> Pending
	reconcileCR(t, r, ns, "run") // Pending + missing CA -> Error
	got := getCR(t, ns, "run")
	if got.Status.Phase != PhaseError {
		t.Fatalf("phase = %q, want Error after missing CA bundle", got.Status.Phase)
	}
	assertFinished(t, got, FinishedReasonTLSCABundleMissing)
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
	mustCreate(t, newScaleValidation(ns, "run", func(cr *v1beta1.ScaleValidation) {
		cr.Spec.Target.TLS = &v1beta1.TLSConfig{
			CABundle: &v1beta1.CABundleSource{
				ConfigMapRef: v1beta1.ConfigMapKeyRef{Name: "internal-ca", Key: "ca.crt"},
			},
		}
	}))

	reconcileCR(t, r, ns, "run")
	reconcileCR(t, r, ns, "run")
	got := getCR(t, ns, "run")
	if got.Status.Phase != PhaseError {
		t.Fatalf("phase = %q, want Error when CA key is missing", got.Status.Phase)
	}
	assertFinished(t, got, FinishedReasonTLSCABundleMissing)
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
	mustCreate(t, newScaleValidation(ns, "run", func(cr *v1beta1.ScaleValidation) {
		cr.Spec.Target.TLS = &v1beta1.TLSConfig{
			CABundle: &v1beta1.CABundleSource{
				ConfigMapRef: v1beta1.ConfigMapKeyRef{Name: "internal-ca", Key: "ca.crt"},
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
		Diagnostics: []v1beta1.DiagnosticAlert{
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
	mustCreate(t, newScaleValidation(ns, "run", func(cr *v1beta1.ScaleValidation) {
		cr.Spec.Target.TLS = &v1beta1.TLSConfig{
			CABundle: &v1beta1.CABundleSource{
				ConfigMapRef: v1beta1.ConfigMapKeyRef{Name: "missing-ca", Key: "ca.crt"},
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
	assertFinished(t, got, FinishedReasonTargetNotReady)
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
	mustCreateReadyTarget(t, ns, "app") // supplies the first healthy pod
	createHealthyTargetPod(t, ns, "app-0", "app")
	mustCreate(t, newScaleValidation(ns, "chaos", func(cr *v1beta1.ScaleValidation) {
		cr.Spec.Disruption = &v1beta1.DisruptionConfig{
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
	mustCreateReadyTarget(t, ns, "app") // supplies the only healthy pod
	mustCreate(t, newScaleValidation(ns, "chaos", func(cr *v1beta1.ScaleValidation) {
		cr.Spec.Disruption = &v1beta1.DisruptionConfig{
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
	mustCreateReadyTarget(t, ns, "app") // supplies the first healthy pod
	createHealthyTargetPod(t, ns, "app-0", "app")
	mustCreate(t, newScaleValidation(ns, "chaos", func(cr *v1beta1.ScaleValidation) {
		cr.Spec.Disruption = &v1beta1.DisruptionConfig{
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
