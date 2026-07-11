# Validate a gRPC Service

An HPA that looks healthy under HTTP/1 synthetic load can still fall over under real gRPC traffic: h2 multiplexing changes connection-pool dynamics, and pod drain surfaces as stream resets rather than connection refusals. This guide validates a gRPC workload end to end.

## Prerequisites

- scale-sentry installed ([Getting Started](../getting-started.md))
- A gRPC target that serves the standard [gRPC Health protocol](https://grpc.io/docs/guides/health-checking/) (most production services do; the loadgen drives unary `Health.Check` RPCs, so no per-service proto stubs are needed)
- metrics-server running (HPAs need it)

No suitable target handy? The repo ships one, CPU-bound with an HPA:

```bash
kubectl apply -f https://raw.githubusercontent.com/ethan-kane-ops/scale-sentry/main/config/e2e/00-namespace.yaml
kubectl apply -f https://raw.githubusercontent.com/ethan-kane-ops/scale-sentry/main/config/e2e/targets/grpc-health.yaml
```

## Run the validation

```yaml
apiVersion: validation.scale-sentry.ek.co/v1alpha1
kind: ScaleValidation
metadata:
  name: grpc-canary
  namespace: scale-sentry-e2e
spec:
  targetRef:
    apiVersion: apps/v1
    kind: Deployment
    name: grpc-health
  sla: 2m
  target:
    mode: ServiceDefault
    port: 50051
    networkPath: ClusterIP
    protocol: GRPC
  load:
    baseRps: 200
    concurrencyFactor: 50
    profile:
      pattern: Poisson
```

Two gRPC-specific knobs:

- `spec.target.protocol: GRPC` switches the loadgen to unary `Health.Check` RPCs over h2. The URL path is ignored; the loadgen dials `host:port`.
- `spec.target.grpc.service` (optional) probes a named service registered with the health server instead of the server-wide default. Set it when one binary hosts several services and you care about a specific one.

Poisson arrivals are the right default for gRPC SLA verdicts: exponential inter-arrival matches real RPC traffic better than a constant tick. See [Load Profiles](../load-profiles.md).

## Read the results

```bash
kubectl -n scale-sentry-e2e get scalevalidation grpc-canary -w
kubectl -n scale-sentry-e2e describe scalevalidation grpc-canary   # lifecycle Events
```

Things worth checking beyond the verdict band:

- `status.failureRate` under drain or disruption: gRPC failures here are stream resets (`UNAVAILABLE`), the signature of a pod terminated without connection draining.
- `status.diagnostics` for `MetricsLikelySkewed`: if leakage or drain findings fired, the latency numbers include error responses and should be read accordingly.

A complete runnable example lives at [`config/e2e/validations/grpc-clusterip.yaml`](https://github.com/ethan-kane-ops/scale-sentry/blob/main/config/e2e/validations/grpc-clusterip.yaml), and the gRPC wire-level details are in [Protocols](../protocols.md).
