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
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
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

func newReconciler() *ScaleValidationReconciler {
	return &ScaleValidationReconciler{
		Client:                 k8sClient,
		Scheme:                 testScheme,
		LoadgenImage:           "test/loadgen:v1",
		ObserverImage:          "test/observer:v1",
		ObserverServiceAccount: "scale-sentry-observer",
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

	// Force the CR into Running without ever spawning a Job — the
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
	mustCreate(t, newTargetDeployment(ns, "app", true))
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
