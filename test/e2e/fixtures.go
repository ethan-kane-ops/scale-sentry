//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/yaml"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
)

// fixtureNamespace is the fixed namespace every config/e2e fixture lives
// in; the matrix scenarios run the exact manifests the fixture README
// tells users to apply, so drift between docs and CI is impossible.
const fixtureNamespace = "scale-sentry-e2e"

// Envoy Gateway coordinates. The Gateway fixture is reconciled by the
// controller the fixture README installs into envoy-gateway-system; the
// provisioned proxy Service/Deployment carry the owning-gateway labels.
const (
	envoyGatewayNamespace = "envoy-gateway-system"
	fixtureGatewayName    = "scale-sentry-eg"
	gatewayReadyAfter     = 4 * time.Minute
)

// skipUnlessFullMatrix gates the protocol / Gateway / disruption scenarios
// to opted-in runs (nightly cron, `just test-e2e-matrix`). The smoke path
// (PR run-e2e label) stays inside the ENG-62 time budget.
func skipUnlessFullMatrix(t *testing.T) {
	t.Helper()
	if os.Getenv("E2E_FULL_MATRIX") == "" {
		t.Skip("set E2E_FULL_MATRIX=1 to run the full scenario matrix")
	}
}

// scaledSLA widens a fixture's real-time SLA window by E2E_SLA_MULTIPLIER,
// absorbing scheduler/scrape jitter on shared CI compute. Unset (the local
// default) leaves the fixture's documented SLA untouched.
func scaledSLA(base time.Duration) time.Duration {
	v := os.Getenv("E2E_SLA_MULTIPLIER")
	if v == "" {
		return base
	}
	mult, err := strconv.ParseFloat(v, 64)
	if err != nil || mult <= 0 {
		return base
	}
	return time.Duration(float64(base) * mult)
}

// repoPath resolves a repo-relative path from the test binary's working
// directory (test/e2e).
func repoPath(parts ...string) string {
	return filepath.Join(append([]string{"..", ".."}, parts...)...)
}

// kubectlApply shells out to `kubectl apply -f path` against the default
// kubeconfig context, the same trust model as installMetricsServer.
func kubectlApply(t *testing.T, ctx context.Context, path string) {
	t.Helper()
	out, err := exec.CommandContext(ctx, "kubectl", "apply", "-f", path).CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl apply %s: %v\n%s", path, err, out)
	}
	t.Logf("applied %s:\n%s", path, out)
}

// applyProtocolFixtures lays down the shared config/e2e namespace and
// target workloads (whoami h1/h2, grpc-health, Services, HPAs) plus the
// observer RBAC the loadgen Job needs there. Idempotent; every scenario
// calls it so test ordering does not matter.
func applyProtocolFixtures(t *testing.T, c client.Client, ctx context.Context) {
	t.Helper()
	kubectlApply(t, ctx, repoPath("config", "e2e", "00-namespace.yaml"))
	kubectlApply(t, ctx, repoPath("config", "e2e", "targets"))
	applyObserverRBAC(t, c, ctx, fixtureNamespace)
}

// loadValidationFixture decodes one config/e2e/validations YAML into a
// typed CR, so the scenario submits exactly what the README documents.
func loadValidationFixture(t *testing.T, name string) *v1alpha1.ScaleValidation {
	t.Helper()
	raw, err := os.ReadFile(repoPath("config", "e2e", "validations", name))
	if err != nil {
		t.Fatalf("read validation fixture: %v", err)
	}
	var cr v1alpha1.ScaleValidation
	if err := yaml.UnmarshalStrict(raw, &cr); err != nil {
		t.Fatalf("decode validation fixture %s: %v", name, err)
	}
	cr.Spec.SLA.Duration = scaledSLA(cr.Spec.SLA.Duration)
	return &cr
}

// gatewayHost applies the Envoy Gateway fixtures and waits for the
// provisioned proxy to come up, returning the in-cluster DNS host of the
// proxy Service. Discovering the Service by its owning-gateway labels
// (rather than trusting the naming scheme hardcoded in the fixture CRs)
// keeps the scenario robust across Envoy Gateway versions.
func gatewayHost(t *testing.T, c client.Client, ctx context.Context) string {
	t.Helper()
	kubectlApply(t, ctx, repoPath("config", "e2e", "envoy-gateway"))

	sel := client.MatchingLabels{
		"gateway.envoyproxy.io/owning-gateway-name":      fixtureGatewayName,
		"gateway.envoyproxy.io/owning-gateway-namespace": fixtureNamespace,
	}
	var svcName string
	if err := waitFor(ctx, gatewayReadyAfter, func() (bool, error) {
		var svcs corev1.ServiceList
		if err := c.List(ctx, &svcs, client.InNamespace(envoyGatewayNamespace), sel); err != nil {
			return false, err
		}
		if len(svcs.Items) == 0 {
			return false, nil
		}
		svcName = svcs.Items[0].Name

		var deps appsv1.DeploymentList
		if err := c.List(ctx, &deps, client.InNamespace(envoyGatewayNamespace), sel); err != nil {
			return false, err
		}
		if len(deps.Items) == 0 {
			return false, nil
		}
		return deps.Items[0].Status.ReadyReplicas >= 1, nil
	}); err != nil {
		t.Fatalf("envoy gateway proxy for %s/%s never became ready: %v",
			fixtureNamespace, fixtureGatewayName, err)
	}
	return fmt.Sprintf("%s.%s.svc.cluster.local", svcName, envoyGatewayNamespace)
}
