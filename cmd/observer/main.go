// Command observer is the cluster-side observation sidecar for a
// scale-sentry validation run. The controller schedules it as a native
// sidecar in the loadgen Job; it observes the target HPA, EndpointSlices,
// cgroup throttling, and pod conditions until SIGTERM (load run exit),
// then prints an observer.Report as JSON to stdout for the controller.
package main

import (
	"context"
	"fmt"
	"os"
	"os/signal"
	"syscall"

	"github.com/spf13/cobra"
	"k8s.io/client-go/rest"

	"github.com/ethan-kane-ops/scale-sentry/internal/observer"
)

func main() {
	var cfg observer.Config
	cmd := &cobra.Command{
		Use:   "observer",
		Short: "scale-sentry cluster-side observation sidecar",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), cfg)
		},
	}

	f := cmd.Flags()
	f.StringVar(&cfg.TargetName, "target-name", "", "name of the target Deployment (required)")
	f.StringVar(&cfg.Namespace, "namespace", "", "namespace of the target Deployment (required)")
	f.StringVar(&cfg.ServiceName, "service-name", "", "Service whose EndpointSlices to watch (default: target name)")
	f.DurationVar(&cfg.SLA, "sla", 0, "HPA scale-up SLA (required)")
	f.DurationVar(&cfg.PollInterval, "poll-interval", 0, "HPA poll cadence (default 5s)")
	f.StringVar(&cfg.ResultFile, "result-file", "", "shared-volume path of the loadgen JSON result")

	_ = cmd.MarkFlagRequired("target-name")
	_ = cmd.MarkFlagRequired("namespace")
	_ = cmd.MarkFlagRequired("sla")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := cmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, cfg observer.Config) error {
	restConfig, err := rest.InClusterConfig()
	if err != nil {
		return fmt.Errorf("in-cluster config: %w", err)
	}
	sess, err := observer.NewSession(cfg, restConfig)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "observer: watching %s/%s, SLA %s\n", cfg.Namespace, cfg.TargetName, cfg.SLA)

	report := sess.Run(ctx)

	line, err := observer.MarshalReportLine(report)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintln(os.Stdout, line); err != nil {
		return fmt.Errorf("write report line: %w", err)
	}
	return nil
}
