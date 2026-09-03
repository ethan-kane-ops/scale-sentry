# Getting Started

## Install

Install via the OCI Helm chart from GHCR:

```bash
helm install scale-sentry \
  oci://ghcr.io/ethan-kane-ops/charts/scale-sentry \
  --namespace scale-sentry --create-namespace
```

Without `--version` this resolves the latest released chart; add `--version X.Y.Z` (matching a [release](https://github.com/ethan-kane-ops/scale-sentry/releases)) to pin for reproducible installs.

!!! tip "Verify before you install"
    Every released image and the chart are cosign-signed. See [Security](security.md) for the verification command.

## Container images

| Image | Role |
|---|---|
| `ghcr.io/ethan-kane-ops/scale-sentry` | controller |
| `ghcr.io/ethan-kane-ops/scale-sentry-loadgen` | load generator job |
| `ghcr.io/ethan-kane-ops/scale-sentry-observer` | observer job |

All images are multi-arch (`linux/amd64`, `linux/arm64`).

## Quickstart: first verdict in five minutes

You need a target Deployment with an HPA, and the cluster needs metrics-server (HPAs cannot act without it; `just dev-up` installs it on the local kind cluster, and [Troubleshooting](troubleshooting.md) covers clusters that lack it). The repo ships [podinfo](https://github.com/stefanprodan/podinfo) (Deployment + Service + HPA) as the canonical demo target:

```bash
kubectl apply -f https://raw.githubusercontent.com/ethan-kane-ops/scale-sentry/main/config/samples/targets/podinfo.yaml
kubectl apply -f https://raw.githubusercontent.com/ethan-kane-ops/scale-sentry/main/config/samples/scalevalidation-servicedefault.yaml
```

Watch the run move through its phases:

```bash
kubectl get scalevalidation podinfo-default -w
```

```
NAME              PHASE       SLA    TRAFFIC   AGE
podinfo-default   Pending                      2s
podinfo-default   Running                      8s
podinfo-default   Succeeded   Pass   Pass      3m41s
```

The controller narrates the lifecycle through Events, so a failing run explains itself:

```bash
kubectl describe scalevalidation podinfo-default
```

Read the full verdict off the status subresource:

```bash
kubectl get scalevalidation podinfo-default -o yaml
```

Key status fields: `phase` (Pending / Running / Succeeded / Failed / Error / Terminating), `slaStatus` and `trafficIntegrity` (Pass / Fail / Unknown), `scaleUpDuration` (measured HPA reaction), `totalRequests` / `failedRequests` / `failureRate`, `diagnostics` (the analyzer findings, each with an alert name and severity), and `history` (the last ten terminal verdicts, newest first, so trend is visible without a metrics stack). `conditions` carries `Finished`, set True the moment a run reaches any terminal phase, which is what makes `kubectl wait --for=condition=Finished` a usable CI gate (see [Gate a Pipeline on a Verdict](guides/ci-gate.md)). The [Events](events.md) page maps every lifecycle transition; [Observability](observability.md) covers the matching Prometheus metrics.

## Sample library

Every spec shape ships as a runnable manifest in [`config/samples/`](https://github.com/ethan-kane-ops/scale-sentry/tree/main/config/samples):

| Sample | Demonstrates |
|---|---|
| `scalevalidation-servicedefault.yaml` | Minimal run: Service endpoint, constant load |
| `scalevalidation-autodiscover.yaml` | `AutoDiscoverProbe` readiness-path targeting |
| `scalevalidation-custompath.yaml` | `CustomPath` explicit endpoint |
| `scalevalidation-rampload.yaml` | `Ramp` open-loop profile with warmup |
| `scalevalidation-poissonload.yaml` | `Poisson` arrivals for SLA-accurate p99 |
| `scalevalidation-http2.yaml` | HTTP/2 (h2c prior-knowledge) target |
| `scalevalidation-grpc.yaml` | gRPC Health/Check load |
| `scalevalidation-tls.yaml` | Private CA bundle via ConfigMap |
| `scalevalidation-with-disruption.yaml` | Chaos pod-kill at peak load |
| `targets/podinfo.yaml` | Demo target: Deployment + Service + HPA |

## Annotation bridge

Skip the manifest entirely: annotate any Deployment to opt into shadow validation. The controller provisions a `ScaleValidation` for you.

```bash
kubectl annotate deployment/payment-service \
  validation.scale-sentry.ek.co/enabled=true \
  validation.scale-sentry.ek.co/sla=90s \
  validation.scale-sentry.ek.co/base-rps=150 \
  validation.scale-sentry.ek.co/port=8080
```

See [Configuration](configuration.md) for every spec field and targeting mode.
