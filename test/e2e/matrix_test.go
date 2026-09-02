//go:build e2e

// Full scenario matrix beyond the smoke path: protocol verdicts (gRPC,
// h2c), the Gateway network path, and the disruption/drain run. Gated
// behind E2E_FULL_MATRIX because the Gateway scenarios need Envoy Gateway
// installed in the cluster (`just envoy-gateway`) and the whole matrix
// blows the PR-label time budget; the nightly cron runs it.
package e2e

import (
	"context"
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
)

const (
	// matrixScenarioTimeout bounds one full fixture run: metrics-server
	// install (first scenario only), target rollout, HPA warm-up, the 2m
	// fixture SLA window, and report collection.
	matrixScenarioTimeout = 15 * time.Minute
	matrixTerminalAfter   = 8 * time.Minute
)

// runValidationFixture submits the CR, waits for the terminal phase, and
// asserts the full verdict (Succeeded / SLA Pass / traffic Pass).
func runValidationFixture(t *testing.T, c client.Client, ctx context.Context, cr *v1alpha1.ScaleValidation) {
	t.Helper()
	mustCreate(t, c, ctx, cr)
	t.Cleanup(func() { _ = c.Delete(context.Background(), cr) })

	phase, err := waitForTerminalPhase(ctx, c, cr.Namespace, cr.Name, matrixTerminalAfter)
	if err != nil {
		dumpCR(t, c, cr.Namespace, cr.Name)
		t.Fatalf("wait for terminal phase: %v", err)
	}
	got := getCR(t, c, cr.Namespace, cr.Name)
	t.Logf("phase=%s slaStatus=%s trafficIntegrity=%s requests=%d failed=%d diagnostics=%d",
		got.Status.Phase, got.Status.SLAStatus, got.Status.TrafficIntegrity,
		got.Status.TotalRequests, got.Status.FailedRequests, len(got.Status.Diagnostics))
	if phase != "Succeeded" || got.Status.SLAStatus != "Pass" || got.Status.TrafficIntegrity != "Pass" {
		dumpCR(t, c, cr.Namespace, cr.Name)
		t.Fatalf("verdict = %s/%s/%s, want Succeeded/Pass/Pass",
			phase, got.Status.SLAStatus, got.Status.TrafficIntegrity)
	}
}

// prepareFixtureTarget applies the shared fixtures and blocks until the
// named workload is ready and its HPA reports live CPU metrics, so the
// SLA window never starts before the scaling subsystem can act.
func prepareFixtureTarget(t *testing.T, c client.Client, ctx context.Context, workload string) {
	t.Helper()
	installMetricsServer(t, ctx)
	// Reset before the apply, not after: the reset deletes the HPA to clear
	// its scale-down stabilisation history, and applyProtocolFixtures then
	// recreates it from the same manifest.
	resetTargetToColdStart(t, c, ctx, workload)
	applyProtocolFixtures(t, c, ctx)
	if err := waitForDeploymentReady(ctx, c, fixtureNamespace, workload, deploymentReadyAfter); err != nil {
		t.Fatalf("target %s not ready: %v", workload, err)
	}
	if err := waitForHPAMetrics(ctx, c, fixtureNamespace, workload, hpaMetricsReadyAfter); err != nil {
		t.Fatalf("HPA %s never got CPU metrics: %v", workload, err)
	}
}

// TestE2E_GRPCVerdict runs the config/e2e gRPC fixture end to end: unary
// Health/Check load against the grpc-health Service ClusterIP, full
// verdict asserted.
func TestE2E_GRPCVerdict(t *testing.T) {
	skipUnlessFullMatrix(t)
	c := newE2EClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), matrixScenarioTimeout)
	defer cancel()

	prepareFixtureTarget(t, c, ctx, "grpc-health")
	runValidationFixture(t, c, ctx, loadValidationFixture(t, "grpc-clusterip.yaml"))
}

// TestE2E_HTTP2Verdict runs the config/e2e h2c fixture end to end: the
// loadgen speaks prior-knowledge HTTP/2 cleartext to the h2c-echo
// Service ClusterIP, full verdict asserted.
func TestE2E_HTTP2Verdict(t *testing.T) {
	skipUnlessFullMatrix(t)
	c := newE2EClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), matrixScenarioTimeout)
	defer cancel()

	prepareFixtureTarget(t, c, ctx, "h2c-echo")
	runValidationFixture(t, c, ctx, loadValidationFixture(t, "http2-clusterip.yaml"))
}

// TestE2E_GatewayVerdict runs the Gateway networkPath fixture: HTTP/1.1
// load routed through the Envoy Gateway edge (HTTPRoute -> whoami-h1),
// with the proxy Service host discovered at runtime instead of trusting
// the naming scheme the fixture hardcodes.
func TestE2E_GatewayVerdict(t *testing.T) {
	skipUnlessFullMatrix(t)
	c := newE2EClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), matrixScenarioTimeout)
	defer cancel()

	prepareFixtureTarget(t, c, ctx, "whoami-h1")
	host := gatewayHost(t, c, ctx)
	t.Logf("gateway host: %s", host)

	cr := loadValidationFixture(t, "http1-gateway.yaml")
	cr.Spec.Target.Host = host
	runValidationFixture(t, c, ctx, cr)
}

// TestE2E_DisruptionDrainDiagnostics runs spec.disruption end to end: two
// healthy replicas, a pod kill at the trigger point, and the observer's
// drain analyzer catching the requests the dying pod failed to drain. The
// run legitimately lands on Failed when the drops push traffic integrity
// over the line; that failure IS the feature working, so both terminal
// verdict phases are accepted and the assertions target the injection
// condition and the UngracefulDrain diagnostic.
func TestE2E_DisruptionDrainDiagnostics(t *testing.T) {
	skipUnlessFullMatrix(t)
	c := newE2EClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), matrixScenarioTimeout)
	defer cancel()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ss-e2e-chaos-"}}
	mustCreate(t, c, ctx, ns)
	t.Cleanup(func() { _ = c.Delete(context.Background(), ns) })
	applyObserverRBAC(t, c, ctx, ns.Name)

	labels := map[string]string{"app": "target"}
	deploy := targetDeployment(ns.Name, labels)
	two := int32(2)
	deploy.Spec.Replicas = &two
	mustCreate(t, c, ctx, deploy)
	mustCreate(t, c, ctx, targetService(ns.Name, labels))
	if err := waitForDeploymentReady(ctx, c, ns.Name, "target", deploymentReadyAfter); err != nil {
		t.Fatalf("target not ready: %v", err)
	}

	cr := &v1alpha1.ScaleValidation{
		ObjectMeta: metav1.ObjectMeta{Name: "disruption", Namespace: ns.Name},
		Spec: v1alpha1.ScaleValidationSpec{
			TargetRef: v1alpha1.CrossVersionObjectReference{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "target",
			},
			// 90s window (scaled by E2E_SLA_MULTIPLIER on CI, see
			// scaledSLA): the kill lands at 30s, leaving at least a full
			// minute of measured traffic to correlate against the
			// endpoint removal.
			SLA:    metav1.Duration{Duration: scaledSLA(90 * time.Second)},
			Target: v1alpha1.TargetConfig{Mode: "ServiceDefault", Port: targetPort, NetworkPath: "ClusterIP"},
			// hpa-example costs ~50ms CPU per request; 20 RPS across two
			// replicas keeps the node comfortable while giving the drain
			// window enough in-flight traffic to catch dropped requests.
			Load: v1alpha1.LoadConfig{BaseRPS: 20, ConcurrencyFactor: 1},
			Disruption: &v1alpha1.DisruptionConfig{
				InjectPodDeletion:   true,
				MinReplicasForChaos: 2,
				TriggerDelay:        &metav1.Duration{Duration: 30 * time.Second},
			},
		},
	}
	mustCreate(t, c, ctx, cr)

	phase, err := waitForTerminalPhase(ctx, c, ns.Name, cr.Name, matrixTerminalAfter)
	if err != nil {
		dumpCR(t, c, ns.Name, cr.Name)
		t.Fatalf("wait for terminal phase: %v", err)
	}
	if phase != "Succeeded" && phase != "Failed" {
		dumpCR(t, c, ns.Name, cr.Name)
		t.Fatalf("phase = %q, want Succeeded or Failed (verdict, not Error)", phase)
	}

	got := getCR(t, c, ns.Name, cr.Name)
	cond := meta.FindStatusCondition(got.Status.Conditions, "DisruptionInjected")
	if cond == nil || cond.Status != metav1.ConditionTrue {
		dumpCR(t, c, ns.Name, cr.Name)
		t.Fatalf("DisruptionInjected condition = %+v, want True (pod deleted)", cond)
	}
	hasDrain := false
	for _, d := range got.Status.Diagnostics {
		if d.Type == "UngracefulDrain" {
			hasDrain = true
		}
	}
	if !hasDrain {
		dumpCR(t, c, ns.Name, cr.Name)
		t.Fatalf("diagnostics carry no UngracefulDrain alert; drain correlation missed the kill")
	}
	t.Logf("disruption verdict: phase=%s traffic=%s failed=%d diagnostics=%d",
		phase, got.Status.TrafficIntegrity, got.Status.FailedRequests, len(got.Status.Diagnostics))
}
