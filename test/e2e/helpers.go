//go:build e2e

package e2e

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/rest"
	"k8s.io/client-go/tools/clientcmd"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
)

const pollInterval = 3 * time.Second

// mustRESTConfig resolves the kubeconfig via the standard precedence:
// $KUBECONFIG (colon-separated files merged), falling back to
// ~/.kube/config. Same behaviour as kubectl, which the test invocations
// also rely on.
func mustRESTConfig(t *testing.T) *rest.Config {
	t.Helper()
	rules := clientcmd.NewDefaultClientConfigLoadingRules()
	cfg, err := clientcmd.NewNonInteractiveDeferredLoadingClientConfig(rules, &clientcmd.ConfigOverrides{}).ClientConfig()
	if err != nil {
		t.Fatalf("load kubeconfig: %v", err)
	}
	return cfg
}

func mustScheme(t *testing.T) *runtime.Scheme {
	t.Helper()
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("add core scheme: %v", err)
	}
	if err := autoscalingv2.AddToScheme(scheme); err != nil {
		t.Fatalf("add autoscaling scheme: %v", err)
	}
	if err := v1alpha1.AddToScheme(scheme); err != nil {
		t.Fatalf("add scale-sentry scheme: %v", err)
	}
	return scheme
}

func newE2EClient(t *testing.T) client.Client {
	t.Helper()
	c, err := client.New(mustRESTConfig(t), client.Options{Scheme: mustScheme(t)})
	if err != nil {
		t.Fatalf("build client: %v", err)
	}
	return c
}

func mustCreate(t *testing.T, c client.Client, ctx context.Context, obj client.Object) {
	t.Helper()
	if err := c.Create(ctx, obj); err != nil && !apierrors.IsAlreadyExists(err) {
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
		time.Sleep(pollInterval)
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
		time.Sleep(pollInterval)
	}
	return "", context.DeadlineExceeded
}

func getCR(t *testing.T, c client.Client, ns, name string) *v1alpha1.ScaleValidation {
	t.Helper()
	var cr v1alpha1.ScaleValidation
	if err := c.Get(context.Background(), types.NamespacedName{Namespace: ns, Name: name}, &cr); err != nil {
		t.Fatalf("get CR: %v", err)
	}
	return &cr
}

// dumpCR pretty-prints the CR status when an assertion fails. Keeps the
// failure message focused while still giving enough context to diagnose
// from CI logs (no kubectl access there).
func dumpCR(t *testing.T, c client.Client, ns, name string) {
	t.Helper()
	cr := getCR(t, c, ns, name)
	cr.SetManagedFields(nil)
	body, err := json.MarshalIndent(cr.Status, "", "  ")
	if err != nil {
		t.Logf("dump CR: marshal failed: %v", err)
		return
	}
	t.Logf("ScaleValidation %s/%s status:\n%s", ns, name, string(body))
}
