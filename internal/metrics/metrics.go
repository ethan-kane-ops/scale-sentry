// Package metrics declares the scale-sentry-specific Prometheus collectors
// that ride on top of the controller-runtime metrics server (`:8080/metrics`
// by default). All collectors register against `ctrlmetrics.Registry` so
// scrape configs pick them up alongside the standard reconciler counters
// (controller_runtime_reconcile_total, etc.).
package metrics

import (
	"github.com/prometheus/client_golang/prometheus"
	ctrlmetrics "sigs.k8s.io/controller-runtime/pkg/metrics"
)

const (
	// VerdictPass / Warn / Fail / Unknown classify a finished run. The
	// strings match what gets written to ScaleValidation.status.slaStatus
	// and traffic-integrity verdicts so dashboards line up with the CR.
	VerdictPass    = "pass"
	VerdictWarn    = "warn"
	VerdictFail    = "fail"
	VerdictUnknown = "unknown"
)

var (
	// RunsTotal counts terminal ScaleValidation runs by verdict, namespace,
	// and name. A run is counted exactly once when it transitions to
	// Succeeded or Failed. namespace/name are bounded by how many
	// ScaleValidation CRs exist in the cluster, not free text, so this
	// stays cardinality-safe: without them, a fleet with more than one
	// target collapses into a single pass/fail blob with no way to tell
	// which target is failing from the metric alone.
	RunsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "scale_sentry_runs_total",
			Help: "Number of ScaleValidation runs that reached a terminal phase, by verdict, namespace, and name.",
		},
		[]string{"namespace", "name", "verdict"},
	)

	// RunDurationSeconds is the wall-clock duration of a completed run,
	// observed at run finalization. Buckets span the typical SLA range
	// (5s through 10min) so a default Grafana panel renders without
	// custom histogram quantile tweaks.
	RunDurationSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "scale_sentry_run_duration_seconds",
			Help:    "Wall-clock duration of a ScaleValidation run, by namespace and name.",
			Buckets: []float64{5, 15, 30, 60, 120, 300, 600},
		},
		[]string{"namespace", "name"},
	)

	// HPAReactSeconds is the time between load start and the first HPA
	// scale-up action, copied from the observer's measurement. Tighter
	// bucket set than RunDuration because operators care about sub-minute
	// detail here.
	HPAReactSeconds = prometheus.NewHistogramVec(
		prometheus.HistogramOpts{
			Name:    "scale_sentry_hpa_react_seconds",
			Help:    "Time from load start to first HPA scale-up action, by namespace and name.",
			Buckets: []float64{1, 5, 10, 20, 30, 60, 120, 300},
		},
		[]string{"namespace", "name"},
	)

	// DiagnosticAlertsTotal counts diagnostic findings emitted by the
	// analyzer pipeline, labelled by alert type, severity, namespace, and
	// name so operators can chart "CFS throttling rising on Warning" for
	// the whole fleet or drill into a single target.
	DiagnosticAlertsTotal = prometheus.NewCounterVec(
		prometheus.CounterOpts{
			Name: "scale_sentry_diagnostic_alerts_total",
			Help: "Diagnostic alerts emitted by the analyzer pipeline, by type, severity, namespace, and name.",
		},
		[]string{"namespace", "name", "alert", "severity"},
	)
)

// init registers the collectors against the controller-runtime metrics
// registry; that registry is what the operator's :8080/metrics endpoint
// serves, so a scrape sees both scale-sentry and stock reconciler metrics.
func init() {
	ctrlmetrics.Registry.MustRegister(
		RunsTotal,
		RunDurationSeconds,
		HPAReactSeconds,
		DiagnosticAlertsTotal,
	)
}

// VerdictFromStatus maps an observer verdict string ("Pass" / "Fail" /
// "Unknown" / "") to the Prometheus label value. Empty or unrecognised
// inputs collapse to VerdictUnknown so the counter stays bounded.
func VerdictFromStatus(slaStatus, trafficIntegrity string) string {
	if slaStatus == "Fail" || trafficIntegrity == "Fail" {
		return VerdictFail
	}
	if slaStatus == "Pass" && trafficIntegrity == "Pass" {
		return VerdictPass
	}
	if slaStatus == "" && trafficIntegrity == "" {
		return VerdictUnknown
	}
	return VerdictWarn
}
