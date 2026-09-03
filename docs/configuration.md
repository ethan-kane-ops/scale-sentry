# Configuration

The `ScaleValidation` Custom Resource is the single source of truth for a validation run. The [API Reference](reference/api.md) documents every field; this page covers the common knobs and how they interact.

## Full example

```yaml
apiVersion: validation.scale-sentry.ek.co/v1alpha1
kind: ScaleValidation
metadata:
  name: billing-service-validation
  namespace: production
spec:
  targetRef:
    apiVersion: apps/v1        # any scalable workload, not just apps/v1
    kind: Deployment           # Deployment | StatefulSet | ReplicaSet | ...
    name: billing-service

  sla: 90s

  target:
    mode: AutoDiscoverProbe    # ServiceDefault | AutoDiscoverProbe | CustomPath
    port: 8080
    networkPath: Gateway       # ClusterIP | Ingress | Gateway
    host: billing.example.com  # optional Host override for edge routing
    protocol: HTTP2            # HTTP1 | HTTP2 | GRPC

  load:
    baseRps: 150
    concurrencyFactor: 50
    warmupDuration: 15s
    profile:
      pattern: Ramp            # Constant | Poisson | Ramp | Step | Spike
      endRps: 600
      rampDuration: 2m

  disruption:
    injectPodDeletion: true
    minReplicasForChaos: 2
    triggerDelay: 30s
```

Every shape below also exists as a runnable manifest in
[`config/samples/`](https://github.com/ethan-kane-ops/scale-sentry/tree/main/config/samples).

## Targeting modes

`spec.target.mode` chooses how the loadgen resolves an endpoint:

| Mode | Behavior |
|---|---|
| `ServiceDefault` | Hit the target Service on its declared port. |
| `AutoDiscoverProbe` | Reuse the target's readiness probe path. |
| `CustomPath` | Drive a specific path set in `spec.target.customPath`. |

## Network path

`spec.target.networkPath` isolates *where* a bottleneck lives:

- `ClusterIP`: hit the Service directly, in-cluster. Pure scaling signal, no edge overhead.
- `Gateway`: route through a Gateway API edge (Envoy Gateway) to include edge overhead in the measurement.
- `Ingress`: classic Ingress controllers; kept for legacy clusters.

`spec.target.host` overrides the URL host when the edge routes by hostname (Gateway listeners, external load balancers). Copy-paste recipes for every protocol and edge combination live in the [Target Cookbook](target-cookbook.md).

## Protocol

`spec.target.protocol` selects the wire protocol the loadgen speaks:

- `HTTP1` (default): fasthttp, best raw throughput for plain HTTP/1.1 backends.
- `HTTP2`: ALPN-negotiated h2 over TLS, prior-knowledge h2c over cleartext.
- `GRPC`: drives the standard gRPC Health/Check method; set `spec.target.grpc.service` to probe a named service instead of the server default.

Stack details and trade-offs are in [Protocols](protocols.md).

## Load

- `spec.load.baseRps`: steady-state requests per second.
- `spec.load.concurrencyFactor`: in-flight connection multiplier driving the ramp.
- `spec.load.warmupDuration`: traffic sent before the SLA window opens; excluded from the verdict so TCP/TLS handshakes, JIT, and cache warmup do not pollute the latency histogram.
- `spec.load.profile`: open-loop arrival shape. `pattern` picks `Constant`, `Poisson`, `Ramp`, `Step`, or `Spike`; pattern-specific knobs (`endRps`, `rampDuration`, `stepRps`, `stepDuration`, `spikes[]`) apply only to their named pattern.

Why open-loop arrival models produce honest p99 numbers is covered in [Load Profiles](load-profiles.md).

## TLS

For `https://` targets fronted by a private CA, point the loadgen at a CA bundle shipped in a ConfigMap:

```yaml
spec:
  target:
    tls:
      caBundle:
        configMapRef:
          name: internal-ca
          key: ca.crt
```

`tls.insecureSkipVerify: true` skips verification entirely: acceptable for lab-only self-signed edges, never for anything shared. Full example: [`scalevalidation-tls.yaml`](https://github.com/ethan-kane-ops/scale-sentry/blob/main/config/samples/scalevalidation-tls.yaml).

## Chaos disruption

Enabling `spec.disruption.injectPodDeletion` terminates one healthy replica at peak load to exercise `terminationGracePeriodSeconds`, `preStop` hooks, and EndpointSlice propagation. `minReplicasForChaos` is a safety floor: chaos is skipped below it. `triggerDelay` offsets the kill from the start of peak load.

The decision is single-shot per run and recorded on the CR as a `DisruptionInjected` status condition: `True` (reason `PodDeleted`) names the victim, `False` (reason `Skipped`) carries why the safety gate refused. The kill also emits a `ChaosInjected` or `ChaosSkipped` Event (see [Events](events.md)). Dropped requests correlated with the resulting endpoint removal surface as an `UngracefulDrain` diagnostic in `status.diagnostics`.

## SLA

`spec.sla` is the HPA scale-up latency budget. The run's verdict band (pass / warn / fail) is computed against it. See [Observability](observability.md) for the emitted metrics.
