// Command loadgen is the entrypoint for the scale-sentry load-generator Job
// image. The controller spawns one Pod per ScaleValidation run, passing the
// fully-resolved URL plus run parameters via flags. The Pod prints a JSON
// summary of [loadgen.Result] to stdout on completion.
package main

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"github.com/spf13/cobra"

	"github.com/ethan-kane-ops/scale-sentry/internal/loadgen"
)

type opts struct {
	url               string
	method            string
	headers           []string
	rps               int
	duration          time.Duration
	concurrency       int
	connectionMode    string
	timeout           time.Duration
	targetMode        string
	networkPath       string
	resultFile        string
	tlsInsecure       bool
	tlsCABundle       string
	phasesJSON        string
}

func main() {
	o := opts{}
	cmd := &cobra.Command{
		Use:   "loadgen",
		Short: "scale-sentry HTTP load generator (run as a Kubernetes Job)",
		RunE: func(cmd *cobra.Command, _ []string) error {
			return run(cmd.Context(), o)
		},
	}

	f := cmd.Flags()
	f.StringVar(&o.url, "url", "", "fully-resolved target URL (required)")
	f.StringVar(&o.method, "method", "GET", "HTTP method")
	f.StringSliceVar(&o.headers, "header", nil, "request header in 'Key: Value' form (repeatable)")
	f.IntVar(&o.rps, "rps", 0, "target requests per second (required)")
	f.DurationVar(&o.duration, "duration", 0, "total run duration (required)")
	f.IntVar(&o.concurrency, "concurrency", 0, "worker goroutine count (default: min(rps, 256))")
	f.StringVar(&o.connectionMode, "connection-mode", "", "KeepAlive | ShortLived (required)")
	f.DurationVar(&o.timeout, "timeout", 5*time.Second, "per-request timeout")
	f.StringVar(&o.targetMode, "target-mode", "", "informational label: ServiceDefault | AutoDiscoverProbe | CustomPath")
	f.StringVar(&o.networkPath, "network-path", "", "informational label: ClusterIP | Ingress")
	f.StringVar(&o.resultFile, "result-file", "", "also write the JSON Result to this path (for the observer sidecar)")
	f.BoolVar(&o.tlsInsecure, "tls-insecure-skip-verify", false, "disable TLS certificate verification (dev / CI only, masks TLS failures)")
	f.StringVar(&o.tlsCABundle, "tls-ca-bundle", "", "path to a PEM-encoded CA bundle to trust (for private ingress CAs)")
	f.StringVar(&o.phasesJSON, "phases", "", "JSON-encoded []loadgen.Phase replacing --rps/--duration with a phased run (warmup, ramp, spike)")

	_ = cmd.MarkFlagRequired("url")
	_ = cmd.MarkFlagRequired("connection-mode")

	ctx, stop := signal.NotifyContext(context.Background(), syscall.SIGINT, syscall.SIGTERM)
	defer stop()

	if err := cmd.ExecuteContext(ctx); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(ctx context.Context, o opts) error {
	headers, err := parseHeaders(o.headers)
	if err != nil {
		return err
	}

	cfg := loadgen.Config{
		URL:                   o.url,
		Method:                o.method,
		Headers:               headers,
		TargetRPS:             o.rps,
		Duration:              o.duration,
		Concurrency:           o.concurrency,
		ConnectionMode:        loadgen.ConnectionMode(o.connectionMode),
		Timeout:               o.timeout,
		TargetMode:            loadgen.TargetMode(o.targetMode),
		NetworkPath:           loadgen.NetworkPath(o.networkPath),
		TLSInsecureSkipVerify: o.tlsInsecure,
		TLSCABundlePath:       o.tlsCABundle,
	}
	if o.phasesJSON != "" {
		if err := json.Unmarshal([]byte(o.phasesJSON), &cfg.Phases); err != nil {
			return fmt.Errorf("parse --phases: %w", err)
		}
	}
	if o.phasesJSON == "" && (o.rps == 0 || o.duration == 0) {
		return fmt.Errorf("either --phases or both --rps and --duration are required")
	}

	g, err := loadgen.New(cfg)
	if err != nil {
		return fmt.Errorf("new generator: %w", err)
	}

	if len(cfg.Phases) > 0 {
		fmt.Fprintf(os.Stderr, "loadgen: %s phased run (%d phases, mode=%s)\n",
			cfg.URL, len(cfg.Phases), cfg.ConnectionMode)
	} else {
		fmt.Fprintf(os.Stderr, "loadgen: %s @ %d RPS for %s (mode=%s)\n",
			cfg.URL, cfg.TargetRPS, cfg.Duration, cfg.ConnectionMode)
	}

	result := g.Run(ctx)

	pretty, err := json.MarshalIndent(result, "", "  ")
	if err != nil {
		return fmt.Errorf("encode result: %w", err)
	}
	if _, err := fmt.Fprintln(os.Stdout, string(pretty)); err != nil {
		return fmt.Errorf("write result to stdout: %w", err)
	}
	if o.resultFile != "" {
		if err := os.WriteFile(o.resultFile, pretty, 0o644); err != nil {
			return fmt.Errorf("write result file %s: %w", o.resultFile, err)
		}
	}
	return nil
}

func parseHeaders(raw []string) (map[string]string, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	out := make(map[string]string, len(raw))
	for _, h := range raw {
		k, v, ok := strings.Cut(h, ":")
		if !ok {
			return nil, fmt.Errorf("header %q is not in 'Key: Value' form", h)
		}
		out[strings.TrimSpace(k)] = strings.TrimSpace(v)
	}
	return out, nil
}
