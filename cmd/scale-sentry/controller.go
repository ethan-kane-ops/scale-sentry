package main

import (
	"fmt"

	"github.com/spf13/cobra"
	"k8s.io/apimachinery/pkg/runtime"
	utilruntime "k8s.io/apimachinery/pkg/util/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/healthz"
	"sigs.k8s.io/controller-runtime/pkg/log/zap"
	metricsserver "sigs.k8s.io/controller-runtime/pkg/metrics/server"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
	"github.com/ethan-kane-ops/scale-sentry/internal/controller"
)

const defaultLoadgenImage = "scale-sentry-loadgen:latest"

func init() {
	rootCmd.AddCommand(newControllerCmd())
}

type controllerOpts struct {
	metricsAddr  string
	probeAddr    string
	leaderElect  bool
	loadgenImage string
	devLogging   bool
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
	f.BoolVar(&o.leaderElect, "leader-elect", false, "enable leader election for controller manager")
	f.StringVar(&o.loadgenImage, "loadgen-image", defaultLoadgenImage, "container image for the spawned loadgen Job")
	f.BoolVar(&o.devLogging, "dev-logging", false, "use development (human-readable) log encoding")
	return cmd
}

func runController(o controllerOpts) error {
	ctrl.SetLogger(zap.New(zap.UseDevMode(o.devLogging)))

	scheme := runtime.NewScheme()
	utilruntime.Must(clientgoscheme.AddToScheme(scheme))
	utilruntime.Must(v1alpha1.AddToScheme(scheme))

	mgr, err := ctrl.NewManager(ctrl.GetConfigOrDie(), ctrl.Options{
		Scheme:                 scheme,
		Metrics:                metricsserver.Options{BindAddress: o.metricsAddr},
		HealthProbeBindAddress: o.probeAddr,
		LeaderElection:         o.leaderElect,
		LeaderElectionID:       "scale-sentry.validation.scale-sentry.ek.co",
	})
	if err != nil {
		return fmt.Errorf("create manager: %w", err)
	}

	if err := (&controller.ScaleValidationReconciler{
		Client:       mgr.GetClient(),
		Scheme:       mgr.GetScheme(),
		LoadgenImage: o.loadgenImage,
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
