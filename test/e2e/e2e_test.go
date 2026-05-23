//go:build e2e

// Package e2e is the scale-sentry Kind-based smoke test. It assumes a Kind
// cluster reachable via the default kubeconfig has the Helm chart installed
// (the `just test-e2e` recipe takes care of that). The test applies a real
// target Deployment + Service in a fresh namespace, creates a
// ScaleValidation, and waits for the controller to drive it to a terminal
// phase. Full verdict assertion (Succeeded + Pass) belongs to ENG-39 — this
// suite only proves the whole Dockerfile + Helm + RBAC + controller +
// loadgen + observer stack wires end-to-end inside a real cluster.
package e2e

import (
	"context"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
)

const (
	observerSAName       = "scale-sentry-observer"
	terminalPhaseTimeout = 5 * time.Minute
	readyTimeout         = 2 * time.Minute
)

func newE2EClient(t *testing.T) client.Client {
	t.Helper()
	cfg, err := clientcmd.BuildConfigFromFlags("", clientcmd.RecommendedHomeFile)
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scale-sentry scheme: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	return c
}

func TestE2E_Smoke(t *testing.T) {
	c := newE2EClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Minute)
	defer cancel()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ss-e2e-"}}
	mustCreate(t, c, ctx, ns)
	t.Cleanup(func() {
		_ = c.Delete(context.Background(), ns)
	})
	t.Logf("test namespace: %s", ns.Name)

	// Observer RBAC must exist in the CR's namespace. The chart installs
	// it in its release namespace; for arbitrary test namespaces we lay
	// down the same SA + Role + RoleBinding here.
	applyObserverRBAC(t, c, ctx, ns.Name)

	labels := map[string]string{"app": "target"}
	mustCreate(t, c, ctx, targetDeployment(ns.Name, labels))
	mustCreate(t, c, ctx, targetService(ns.Name, labels))
	if err := waitForDeploymentReady(ctx, c, ns.Name, "target", readyTimeout); err != nil {
		t.Fatalf("target deployment not ready: %v", err)
	}

	cr := &v1alpha1.ScaleValidation{
		ObjectMeta: metav1.ObjectMeta{Name: "smoke", Namespace: ns.Name},
		Spec: v1alpha1.ScaleValidationSpec{
			TargetRef: v1alpha1.CrossVersionObjectReference{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "target",
			},
			SLA:    metav1.Duration{Duration: 60 * time.Second},
			Target: v1alpha1.TargetConfig{Mode: "ServiceDefault", Port: 9898, NetworkPath: "ClusterIP"},
			Load:   v1alpha1.LoadConfig{BaseRPS: 50, ConcurrencyFactor: 5},
		},
	}
	mustCreate(t, c, ctx, cr)

	phase, err := waitForTerminalPhase(ctx, c, ns.Name, cr.Name, terminalPhaseTimeout)
	if err != nil {
		t.Fatalf("wait for terminal phase: %v", err)
	}
	t.Logf("validation finished with phase=%s", phase)

	// Smoke test: any terminal phase proves the end-to-end stack wires.
	// Verdict assertion (Succeeded + Pass) lives in ENG-39.
	switch phase {
	case "Succeeded", "Failed", "Error":
		// ok
	default:
		t.Fatalf("unexpected terminal phase: %q", phase)
	}
}

// targetDeployment builds a small HTTP workload (podinfo) the validation
// run can drive load against. podinfo is widely available, has a working
// /readyz endpoint, and exits cleanly on SIGTERM.
func targetDeployment(ns string, labels map[string]string) *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: ns, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "podinfo",
					Image: "ghcr.io/stefanprodan/podinfo:6.6.2",
					Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 9898}},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							HTTPGet: &corev1.HTTPGetAction{Path: "/readyz", Port: intstr.FromInt32(9898)},
						},
						PeriodSeconds:    5,
						FailureThreshold: 3,
					},
				}}},
			},
		},
	}
}

func targetService(ns string, labels map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: ns, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports:    []corev1.ServicePort{{Port: 9898, TargetPort: intstr.FromInt32(9898)}},
		},
	}
}

// applyObserverRBAC mirrors charts/scale-sentry/templates/observer-rbac.yaml
// into the given namespace.
func applyObserverRBAC(t *testing.T, c client.Client, ctx context.Context, ns string) {
	t.Helper()
	mustCreate(t, c, ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: observerSAName, Namespace: ns},
	})
	mustCreate(t, c, ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: observerSAName, Namespace: ns},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"apps"}, Resources: []string{"deployments"}, Verbs: []string{"get"}},
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}},
			{APIGroups: []string{""}, Resources: []string{"pods/exec"}, Verbs: []string{"create"}},
			{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, Verbs: []string{"list", "watch"}},
			{APIGroups: []string{"autoscaling"}, Resources: []string{"horizontalpodautoscalers"}, Verbs: []string{"get", "list"}},
		},
	})
	mustCreate(t, c, ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: observerSAName, Namespace: ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: observerSAName},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: observerSAName, Namespace: ns}},
	})
}

func mustCreate(t *testing.T, c client.Client, ctx context.Context, obj client.Object) {
	t.Helper()
	if err := c.Create(ctx, obj); err != nil {
		t.Fatalf("create %T %s: %v", obj, obj.GetName(), err)
	}
}

func waitForDeploymentReady(ctx context.Context, c client.Client, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var d appsv1.Deployment
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &d); err != nil {
			return err
		}
		desired := int32(1)
		if d.Spec.Replicas != nil {
			desired = *d.Spec.Replicas
		}
		if d.Status.ReadyReplicas >= desired {
			return nil
		}
		time.Sleep(2 * time.Second)
	}
	return context.DeadlineExceeded
}

// waitForTerminalPhase polls the CR's status.phase until it reaches one of
// the terminal values (Succeeded / Failed / Error) or timeout elapses.
func waitForTerminalPhase(ctx context.Context, c client.Client, ns, name string, timeout time.Duration) (string, error) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var cr v1alpha1.ScaleValidation
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &cr); err != nil {
			return "", err
		}
		switch cr.Status.Phase {
		case "Succeeded", "Failed", "Error":
			return cr.Status.Phase, nil
		}
		time.Sleep(3 * time.Second)
	}
	return "", context.DeadlineExceeded
}
