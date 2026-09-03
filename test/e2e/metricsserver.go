//go:build e2e

package e2e

import (
	"bytes"
	"context"
	_ "embed"
	"os/exec"
	"testing"
	"time"

	"k8s.io/apimachinery/pkg/types"
	apiregv1 "k8s.io/kube-aggregator/pkg/apis/apiregistration/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

//go:embed manifests/metrics-server.yaml
var metricsServerManifest []byte

const (
	metricsAPIService    = "v1beta1.metrics.k8s.io"
	metricsReadyDeadline = 3 * time.Minute
)

// installMetricsServer applies the vendored metrics-server manifest and
// waits for the aggregated API to report Available=True. It is safe to
// run repeatedly; kubectl apply is idempotent and the wait short-circuits
// once the APIService is already serving.
func installMetricsServer(t *testing.T, ctx context.Context) {
	t.Helper()

	cmd := exec.CommandContext(ctx, "kubectl", "apply", "-f", "-")
	cmd.Stdin = bytes.NewReader(metricsServerManifest)
	out, err := cmd.CombinedOutput()
	if err != nil {
		t.Fatalf("kubectl apply metrics-server: %v\n%s", err, string(out))
	}
	t.Logf("metrics-server applied:\n%s", string(out))

	c := newAggClient(t)
	deadline := time.Now().Add(metricsReadyDeadline)
	for time.Now().Before(deadline) {
		var svc apiregv1.APIService
		if err := c.Get(ctx, types.NamespacedName{Name: metricsAPIService}, &svc); err == nil {
			for _, cond := range svc.Status.Conditions {
				if cond.Type == apiregv1.Available && cond.Status == apiregv1.ConditionTrue {
					t.Logf("metrics-server APIService Available")
					return
				}
			}
		}
		time.Sleep(pollInterval)
	}
	t.Fatalf("metrics-server APIService %s never became Available", metricsAPIService)
}

func newAggClient(t *testing.T) client.Client {
	t.Helper()
	cfg := mustRESTConfig(t)
	scheme := mustScheme(t)
	if err := apiregv1.AddToScheme(scheme); err != nil {
		t.Fatalf("add apiregistration scheme: %v", err)
	}
	c, err := client.New(cfg, client.Options{Scheme: scheme})
	if err != nil {
		t.Fatalf("build aggregation client: %v", err)
	}
	return c
}
