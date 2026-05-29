# Scale Sentry

[![CI](https://github.com/ethan-kane-ops/scale-sentry/actions/workflows/ci.yml/badge.svg)](https://github.com/ethan-kane-ops/scale-sentry/actions/workflows/ci.yml)
[![codecov](https://codecov.io/gh/ethan-kane-ops/scale-sentry/branch/main/graph/badge.svg)](https://codecov.io/gh/ethan-kane-ops/scale-sentry)
[![Release](https://github.com/ethan-kane-ops/scale-sentry/actions/workflows/release.yml/badge.svg)](https://github.com/ethan-kane-ops/scale-sentry/actions/workflows/release.yml)
[![License](https://img.shields.io/badge/License-Apache_2.0-blue.svg)](https://opensource.org/licenses/Apache-2.0)
[![Go Report Card](https://goreportcard.com/badge/github.com/ethan-kane-ops/scale-sentry)](https://goreportcard.com/report/github.com/ethan-kane-ops/scale-sentry)
[![Go Reference](https://pkg.go.dev/badge/github.com/ethan-kane-ops/scale-sentry.svg)](https://pkg.go.dev/github.com/ethan-kane-ops/scale-sentry)
[![Kubernetes](https://img.shields.io/badge/Kubernetes-%E2%89%A5%201.26-blue.svg)](https://kubernetes.io)
[![OpenSSF Scorecard](https://api.securityscorecards.dev/projects/github.com/ethan-kane-ops/scale-sentry/badge)](https://securityscorecards.dev/viewer/?uri=github.com/ethan-kane-ops/scale-sentry)

**Scale Sentry** validates Kubernetes auto-scaling behavior under load. It generates dynamic traffic to a target Deployment, tracks HPA scale-up latency against an SLA, and correlates HTTP errors with EndpointSlice updates to surface **cold-start traffic leakage**, errors served the instant a new pod is declared Ready.

It is a [kubebuilder](https://book.kubebuilder.io/) v4 controller built on `controller-runtime`. Workloads are validated declaratively through a `ScaleValidation` Custom Resource or through annotations on existing Deployments.

---

## Architecture

```mermaid
flowchart LR
    User([Platform Engineer]) -->|kubectl apply| CR[ScaleValidation CR]

    subgraph Operator [scale-sentry namespace]
        Controller[Controller]
    end

    CR -.watched.-> Controller
    Controller -->|1 spawn| Loadgen[Loadgen Job]
    Controller -->|2 spawn| Observer[Observer Job]

    subgraph Target [target namespace]
        Service[Service]
        Pods[Target Pods]
        HPA[HorizontalPodAutoscaler]
    end

    Loadgen -->|3 HTTP traffic| Service --> Pods
    Pods -.resource usage.-> HPA
    HPA -.scales.-> Pods

    Observer -.watches HPA / Endpoints / Pods.-> Target
    Observer -.scrapes cAdvisor.-> Pods
    Loadgen -.report.-> Volume[(shared volume)]
    Volume -.read.-> Observer

    Observer -->|4 verdict report| Controller
    Controller -->|5 status update| CR
```

1. Controller reconciles a `ScaleValidation` CR, resolving its `targetRef` and computing dynamic load characteristics.
2. Two jobs are spawned: a Loadgen that drives HTTP traffic and an Observer that watches cluster state and scrapes cgroup metrics.
3. The Observer correlates the Loadgen request log with EndpointSlice updates and emits a structured verdict.
4. The verdict is written back to the CR's `status` subresource, including HPA latency, throttling, leakage diagnostics, and a pass / fail / warning band.

---

## Features

- **Custom Resource driven**, `validation.scale-sentry.ek.co/v1alpha1` `ScaleValidation` resource stores test configuration, SLA targets, and execution history in the resource's `status` subresource.
- **Annotation bridge**, annotating an existing `Deployment` with `validation.scale-sentry.ek.co/enabled=true` provisions a shadow `ScaleValidation` automatically, no manifests required.
- **Endpoint targeting modes**, three target resolution strategies (`ServiceDefault`, `AutoDiscoverProbe`, `CustomPath`) and two network paths (direct `ClusterIP` or `Ingress` gateway) to isolate scaling bottlenecks from edge bottlenecks.
- **Chaos disruption**, optionally terminates a healthy replica at peak load to test `terminationGracePeriodSeconds`, `preStop` hooks, and EndpointSlice propagation delays.
- **Diagnostic suite**:
  - **Readiness lag analyzer**, measures `PodRunning` → `PodReady` delta to detect sparse probe sampling.
  - **TCP / TLS handshake tester**, short-lived versus persistent connection pools.
  - **cgroup throttle watcher**, scrapes `nr_throttled` / `nr_periods` via Kubelet cAdvisor to flag CFS quota throttling.
  - **DNS + PDB auditor**, flags `ndots:5` resolver pressure and missing `PodDisruptionBudget`s.

---

## Custom Resource

```yaml
apiVersion: validation.scale-sentry.ek.co/v1alpha1
kind: ScaleValidation
metadata:
  name: billing-service-validation
  namespace: production
spec:
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: billing-service

  sla: 90s

  target:
    mode: AutoDiscoverProbe   # ServiceDefault | AutoDiscoverProbe | CustomPath
    customPath: /api/v1/checkout
    port: 8080
    networkPath: Ingress      # ClusterIP | Ingress

  load:
    baseRps: 150
    concurrencyFactor: 50

  disruption:
    injectPodDeletion: true
    minReplicasForChaos: 2
    triggerDelay: 30s
```

---

## Install

Install via OCI Helm chart from GHCR:

```bash
helm install scale-sentry \
  oci://ghcr.io/ethan-kane-ops/charts/scale-sentry \
  --version 0.1.0 \
  --namespace scale-sentry --create-namespace
```

Annotate any Deployment to opt into shadow validation:

```bash
kubectl annotate deployment/payment-service \
  validation.scale-sentry.ek.co/enabled=true \
  validation.scale-sentry.ek.co/sla=90s \
  validation.scale-sentry.ek.co/base-rps=150 \
  validation.scale-sentry.ek.co/port=8080
```

Available container images:

- `ghcr.io/ethan-kane-ops/scale-sentry`, controller
- `ghcr.io/ethan-kane-ops/scale-sentry-loadgen`, load generator job
- `ghcr.io/ethan-kane-ops/scale-sentry-observer`, observer job

All images are multi-arch (`linux/amd64`, `linux/arm64`).

### Verifying signatures

Every released image and the OCI chart are signed with [cosign](https://docs.sigstore.dev/)
keyless signing. The signing identity is the GitHub Actions OIDC token bound to
this repository's release workflow, so there are no keys to distribute. Verify
provenance before installing:

```bash
cosign verify ghcr.io/ethan-kane-ops/scale-sentry:v0.1.1 \
  --certificate-identity-regexp 'https://github.com/ethan-kane-ops/scale-sentry/.+' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The same command verifies the loadgen and observer images, and the chart at
`ghcr.io/ethan-kane-ops/charts/scale-sentry`.

### High availability

The controller uses `controller-runtime` leader election (a
`coordination.k8s.io/Lease` in the release namespace), so it is safe to run more
than one replica. Only the leader reconciles; standbys idle until the lease
expires. Enable HA by raising the replica count:

```bash
helm upgrade --install scale-sentry oci://ghcr.io/ethan-kane-ops/charts/scale-sentry \
  --set controller.replicaCount=2
```

With `replicaCount > 1` the chart also renders a `PodDisruptionBudget`
(`minAvailable: 1`) and zone `topologySpreadConstraints`.

Tradeoff: HA adds one Lease object and a brief reconcile gap on failover. When
the leader pod dies, a standby acquires the lease within the renew window
(~15s default) before resuming. A single replica is fine for non-prod; set
`--leader-elect=false` (or `controller.leaderElect=false`) for local
single-node dev to skip the Lease entirely.

---

## Observability

The controller exposes Prometheus metrics on `:8080/metrics`. Stock
controller-runtime reconciler metrics (`controller_runtime_reconcile_*`,
`workqueue_*`) ship alongside the scale-sentry custom collectors:

| Metric | Type | Labels | Purpose |
|---|---|---|---|
| `scale_sentry_runs_total` | counter | `verdict=pass\|warn\|fail\|unknown` | Terminal-run verdict distribution |
| `scale_sentry_run_duration_seconds` | histogram | (none) | Wall-clock duration of a finished run |
| `scale_sentry_hpa_react_seconds` | histogram | (none) | First HPA scale-up reaction latency |
| `scale_sentry_diagnostic_alerts_total` | counter | `alert`, `severity` | Findings emitted by the analyzer pipeline |

To wire prometheus-operator scraping, set both gates in the chart:

```bash
helm upgrade --install scale-sentry oci://ghcr.io/ethan-kane-ops/charts/scale-sentry \
  --set metrics.service.enabled=true \
  --set metrics.serviceMonitor.enabled=true
```

The `metrics.service` block enables a ClusterIP fronting `:8080`; the
`metrics.serviceMonitor` block adds a `monitoring.coreos.com/v1`
ServiceMonitor pointing at it. Both default to off so the chart works on
clusters without prometheus-operator installed; raw `curl :8080/metrics`
still works without either gate.

A starter Grafana dashboard ships at
[`dashboards/scale-sentry.json`](dashboards/scale-sentry.json), import it
via Grafana's `+ -> Import` flow.

---

## Local Development

### Prerequisites

- Go ≥ 1.25 (toolchain pin in `go.mod`)
- [mise](https://mise.jdx.dev/installing-mise.html), runtime + tool manager
- [just](https://just.systems/man/en/), task runner (provisioned by `mise install`)
- A local Kubernetes cluster, [Kind](https://kind.sigs.k8s.io/) or [Minikube](https://minikube.sigs.k8s.io/)

### Bring up a dev cluster

```bash
mise install              # provisions Go, kubectl, helm, kind, just, kubeconform
just dev-up               # creates Kind cluster, builds + loads images, installs the chart
```

Apply a sample CR:

```bash
kubectl apply -f config/samples/targets/podinfo.yaml
kubectl apply -f config/samples/scalevalidation-servicedefault.yaml
```

Tear down:

```bash
just dev-down
```

### Common tasks

| Command                | Purpose                                                 |
|------------------------|---------------------------------------------------------|
| `just check`           | tidy + lint + unit tests, required before every commit |
| `just test-integration`| envtest suite (downloads apiserver + etcd assets)       |
| `just test-e2e`        | full verdict E2E in Kind                                |
| `just generate`        | regenerate `zz_generated.deepcopy.go`                   |
| `just manifests`       | regenerate CRD + RBAC YAML from kubebuilder markers     |

Run `just generate && just manifests` after any change to `api/v1alpha1/*_types.go`.

---

## Contributing

See [CONTRIBUTING.md](./CONTRIBUTING.md) for development setup, coding guidelines, and the pull request workflow. Bug reports and feature requests go through the [issue templates](./.github/ISSUE_TEMPLATE/).

## Security

Vulnerabilities should be reported privately via [GitHub Security Advisory](https://github.com/ethan-kane-ops/scale-sentry/security/advisories/new). See [SECURITY.md](./SECURITY.md) for the disclosure policy.

## License

Apache License 2.0. See [LICENSE](./LICENSE).
