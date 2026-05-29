# Observability

The controller exposes Prometheus metrics on `:8080/metrics`. Stock controller-runtime reconciler metrics (`controller_runtime_reconcile_*`, `workqueue_*`) ship alongside the scale-sentry custom collectors:

| Metric | Type | Labels | Purpose |
|---|---|---|---|
| `scale_sentry_runs_total` | counter | `verdict=pass\|warn\|fail\|unknown` | Terminal-run verdict distribution |
| `scale_sentry_run_duration_seconds` | histogram | (none) | Wall-clock duration of a finished run |
| `scale_sentry_hpa_react_seconds` | histogram | (none) | First HPA scale-up reaction latency |
| `scale_sentry_diagnostic_alerts_total` | counter | `alert`, `severity` | Findings emitted by the analyzer pipeline |

## Prometheus Operator

To wire prometheus-operator scraping, set both gates in the chart:

```bash
helm upgrade --install scale-sentry oci://ghcr.io/ethan-kane-ops/charts/scale-sentry \
  --set metrics.service.enabled=true \
  --set metrics.serviceMonitor.enabled=true
```

The `metrics.service` block enables a ClusterIP fronting `:8080`; the `metrics.serviceMonitor` block adds a `monitoring.coreos.com/v1` ServiceMonitor pointing at it. Both default to off so the chart works on clusters without prometheus-operator installed. Raw `curl :8080/metrics` still works without either gate.

## Grafana

A starter Grafana dashboard ships at [`dashboards/scale-sentry.json`](https://github.com/ethan-kane-ops/scale-sentry/blob/main/dashboards/scale-sentry.json). Import it via Grafana's `+ -> Import` flow.
