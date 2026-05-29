# Getting Started

## Install

Install via the OCI Helm chart from GHCR:

```bash
helm install scale-sentry \
  oci://ghcr.io/ethan-kane-ops/charts/scale-sentry \
  --version 0.1.0 \
  --namespace scale-sentry --create-namespace
```

!!! tip "Verify before you install"
    Every released image and the chart are cosign-signed. See [Security](security.md) for the verification command.

## Container images

| Image | Role |
|---|---|
| `ghcr.io/ethan-kane-ops/scale-sentry` | controller |
| `ghcr.io/ethan-kane-ops/scale-sentry-loadgen` | load generator job |
| `ghcr.io/ethan-kane-ops/scale-sentry-observer` | observer job |

All images are multi-arch (`linux/amd64`, `linux/arm64`).

## Run a validation

Apply a `ScaleValidation` CR against a target Deployment:

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
    mode: AutoDiscoverProbe
    port: 8080
  load:
    baseRps: 150
    concurrencyFactor: 50
```

Watch the verdict land on the resource status:

```bash
kubectl get scalevalidation billing-service-validation -n production -o yaml
```

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
