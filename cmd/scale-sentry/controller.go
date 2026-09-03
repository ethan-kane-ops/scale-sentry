package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	"k8s.io/client-go/kubernetes"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
	"github.com/ethan-kane-ops/scale-sentry/internal/controller"
)

const (
	defaultLoadgenImage         = "scale-sentry-loadgen:latest"
	defaultObserverImage        = "scale-sentry-observer:latest"
	defaultObserverServiceAccnt = "scale-sentry-observer"
	leaderElectionID            = "scale-sentry-leader"
)

func init() {
	rootCmd.AddCommand(newControllerCmd())
}

type controllerOpts struct {
	metricsAddr            string
	probeAddr              string
	leaderElect            bool
	leaderElectNamespace   string
	loadgenImage           string
	observerImage          string
	observerServiceAccount string
	imagePullSecrets       []string
	devLogging             bool
}

func newControllerCmd() *cobra.Command {
	o := controllerOpts{}
	cmd := &cobra.Command{
		Use:   "controller",
		Short: "Run the scale-sentry controller manager",
		RunE: func(_ *cobra.Command, _ []string) error {
			return runController(o)
		},
	}
	f := cmd.Flags()
	f.StringVar(&o.metricsAddr, "metrics-bind-address", ":8080", "address the metrics endpoint binds to")
	f.StringVar(&o.probeAddr, "health-probe-bind-address", ":8081", "address the health probe endpoint binds to")
	f.BoolVar(&o.leaderElect, "leader-elect", true, "enable leader election so only one replica reconciles at a time; set false for single-replica/local dev")
	f.StringVar(&o.leaderElectNamespace, "leader-elect-namespace", "", "namespace holding the leader-election Lease (empty = in-cluster pod namespace)")
	f.StringVar(&o.loadgenImage, "loadgen-image", defaultLoadgenImage, "container image for the loadgen container")
	f.StringVar(&o.observerImage, "observer-image", defaultObserverImage, "container image for the observer sidecar")
	f.StringVar(&o.observerServiceAccount, "observer-service-account", defaultObserverServiceAccnt, "ServiceAccount the loadgen Job pod runs as")
	f.StringSliceVar(&o.imagePullSecrets, "image-pull-secret", nil,
		"imagePullSecret name to set on loadgen/observer Job pods; repeatable, needed when the images live in a private registry")
	f.BoolVar(&o.devLogging, "dev-logging", false, "use development (human-readable) log encoding")
	return cmd
}

func runController(o controllerOpts) error {
	ctrl.SetLogger(zap.New(zap.UseDevMode(o.devLogging)))

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	restCfg := ctrl.GetConfigOrDie()

	mgr, err := ctrl.NewManager(restCfg, ctrl.Options{
		Scheme:                  scheme,
		Metrics:                 metricsserver.Options{BindAddress: o.metricsAddr},
		HealthProbeBindAddress:  o.probeAddr,
		LeaderElection:          o.leaderElect,
		LeaderElectionID:        leaderElectionID,
		LeaderElectionNamespace: o.leaderElectNamespace,
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	clientset, err := kubernetes.NewForConfig(restCfg)
	if err != nil {
		return fmt.Errorf("build clientset: %w", err)
	}

	if err := (&controller.ScaleValidationReconciler{
		Client:                 mgr.GetClient(),
		Scheme:                 mgr.GetScheme(),
		Clientset:              clientset,
		LoadgenImage:           o.loadgenImage,
		ObserverImage:          o.observerImage,
		ObserverServiceAccount: o.observerServiceAccount,
		ImagePullSecrets:       o.imagePullSecrets,
		// GetEventRecorderFor is marked deprecated by controller-runtime in
		// favour of the new events.v1 API, but the new API still requires
		// extra per-event fields (action, related object) that don't fit
		// the simple `kubectl describe` UX we want here. cert-manager and
		// karpenter still use the legacy API for the same reason. Revisit
		// when controller-runtime actually removes the symbol.
		Recorder: mgr.GetEventRecorderFor("scale-sentry"), //nolint:staticcheck
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up scalevalidation controller: %w", err)
	}
	if err := (&controller.DeploymentShadowReconciler{
		Client: mgr.GetClient(),
		Scheme: mgr.GetScheme(),
	}).SetupWithManager(mgr); err != nil {
		return fmt.Errorf("set up deployment-shadow controller: %w", err)
	}

	if err := mgr.AddHealthzCheck("healthz", healthz.Ping); err != nil {
		return fmt.Errorf("add healthz check: %w", err)
	}
	if err := mgr.AddReadyzCheck("readyz", healthz.Ping); err != nil {
		return fmt.Errorf("add readyz check: %w", err)
	}

	if err := mgr.Start(ctrl.SetupSignalHandler()); err != nil {
		return fmt.Errorf("run manager: %w", err)
	}
	return nil
}
