//go:build envtest

// The integration suite runs against a real apiserver + etcd booted by
// envtest. It is gated behind the `envtest` build tag so the hermetic
// `just test` / `just check` run is unaffected; `just test-integration`
// runs it with the downloaded apiserver assets.
package controller

import (
	"os"
	"path/filepath"
	"testing"

	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/envtest"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
)

var (
	testEnv    *envtest.Environment
	k8sClient  client.Client
	testScheme *runtime.Scheme
)

// TestMain boots envtest, installs the CRD, and exposes a direct client
// for the integration tests, then tears the environment down.
func TestMain(m *testing.M) {
	testScheme = runtime.NewScheme()
	must(clientgoscheme.AddToScheme(testScheme))
	must(v1alpha1.AddToScheme(testScheme))

	testEnv = &envtest.Environment{
		CRDDirectoryPaths:     []string{filepath.Join("..", "..", "config", "crd", "bases")},
		ErrorIfCRDPathMissing: true,
	}
	cfg, err := testEnv.Start()
	if err != nil {
		panic("start envtest (is KUBEBUILDER_ASSETS set? run `just test-integration`): " + err.Error())
	}

	k8sClient, err = client.New(cfg, client.Options{Scheme: testScheme})
	if err != nil {
		_ = testEnv.Stop()
		panic("build envtest client: " + err.Error())
	}

	code := m.Run()
	if err := testEnv.Stop(); err != nil {
		panic("stop envtest: " + err.Error())
	}
	os.Exit(code)
}

func must(err error) {
	if err != nil {
		panic(err)
	}
}
