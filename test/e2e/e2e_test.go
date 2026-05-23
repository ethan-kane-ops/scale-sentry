//go:build e2e

// Package e2e is the scale-sentry Kind-based Full E2E. It assumes a Kind
// cluster reachable via the default kubeconfig with the operator Helm
// chart already installed (the `just test-e2e` recipe takes care of that).
//
// The test installs metrics-server (patched for Kind's self-signed kubelet
// certs), brings up the canonical Kubernetes hpa-example workload behind
// a Service + HPA, creates a ScaleValidation with a generous SLA, and
// asserts the controller drives the run to `phase=Succeeded`,
// `slaStatus=Pass`, and `trafficIntegrity=Pass`. Anything weaker would
// let a broken HPA or metrics pipeline slip through, which is exactly
// what ENG-39 is meant to catch.
package e2e

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
)

const (
	// SLA is the window the operator gives the HPA to fully settle.
	// Kind needs: ~30s for metrics-server to start, ~15s for the first
	// kubelet scrape, ~15s HPA sync interval, then time for the new pods
	// to come up and the load to spread. 5 min is comfortably above that
	// while still catching a genuinely broken scale path.
	sla = 5 * time.Minute

	// Test-wide deadlines.
	overallTimeout       = 12 * time.Minute
	deploymentReadyAfter = 2 * time.Minute
	hpaMetricsReadyAfter = 3 * time.Minute
	terminalPhaseAfter   = 8 * time.Minute
)

func TestE2E_FullVerdict(t *testing.T) {
	c := newE2EClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), overallTimeout)
	defer cancel()

	// metrics-server is cluster-scoped; install once, reuse across runs.
	installMetricsServer(t, ctx)

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ss-e2e-"}}
	mustCreate(t, c, ctx, ns)
	t.Cleanup(func() { _ = c.Delete(context.Background(), ns) })
	t.Logf("test namespace: %s", ns.Name)

	applyObserverRBAC(t, c, ctx, ns.Name)

	labels := map[string]string{"app": "target"}
	mustCreate(t, c, ctx, targetDeployment(ns.Name, labels))
	mustCreate(t, c, ctx, targetService(ns.Name, labels))
	mustCreate(t, c, ctx, targetHPA(ns.Name))

	if err := waitForDeploymentReady(ctx, c, ns.Name, "target", deploymentReadyAfter); err != nil {
		t.Fatalf("target not ready: %v", err)
	}
	if err := waitForHPAMetrics(ctx, c, ns.Name, "target", hpaMetricsReadyAfter); err != nil {
		t.Fatalf("HPA never got CPU metrics: %v", err)
	}

	cr := &v1alpha1.ScaleValidation{
		ObjectMeta: metav1.ObjectMeta{Name: "fullverdict", Namespace: ns.Name},
		Spec: v1alpha1.ScaleValidationSpec{
			TargetRef: v1alpha1.CrossVersionObjectReference{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "target",
			},
			SLA:    metav1.Duration{Duration: sla},
			Target: v1alpha1.TargetConfig{Mode: "ServiceDefault", Port: targetPort, NetworkPath: "ClusterIP"},
			// hpa-example does ~50ms of CPU per request (PHP sqrt loop).
			// 1 replica at 200m sustains ~4 RPS at 100% CPU. At 10-14
			// RPS the HPA settles around 3-4 replicas, well above the
			// scale-out trigger but below maxReplicas=5. ConcurrencyFactor=1
			// keeps the per-core uplift predictable across CI runners
			// and laptops (no scheduler stampede on under-provisioned VMs).
			Load: v1alpha1.LoadConfig{BaseRPS: 10, ConcurrencyFactor: 1},
		},
	}
	mustCreate(t, c, ctx, cr)

	phase, err := waitForTerminalPhase(ctx, c, ns.Name, cr.Name, terminalPhaseAfter)
	if err != nil {
		dumpCR(t, c, ns.Name, cr.Name)
		t.Fatalf("wait for terminal phase: %v", err)
	}

	got := getCR(t, c, ns.Name, cr.Name)
	scaleUp := "n/a"
	if got.Status.ScaleUpDuration != nil {
		scaleUp = got.Status.ScaleUpDuration.Duration.String()
	}
	t.Logf("phase=%s slaStatus=%s trafficIntegrity=%s scaleUp=%s requests=%d failed=%d diagnostics=%d",
		got.Status.Phase, got.Status.SLAStatus, got.Status.TrafficIntegrity, scaleUp,
		got.Status.TotalRequests, got.Status.FailedRequests, len(got.Status.Diagnostics))

	if phase != "Succeeded" {
		dumpCR(t, c, ns.Name, cr.Name)
		t.Fatalf("phase = %q, want Succeeded", phase)
	}
	if got.Status.SLAStatus != "Pass" {
		dumpCR(t, c, ns.Name, cr.Name)
		t.Fatalf("slaStatus = %q, want Pass", got.Status.SLAStatus)
	}
	if got.Status.TrafficIntegrity != "Pass" {
		dumpCR(t, c, ns.Name, cr.Name)
		t.Fatalf("trafficIntegrity = %q, want Pass", got.Status.TrafficIntegrity)
	}
}
