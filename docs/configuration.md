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

## Targeting modes

`spec.target.mode` chooses how the loadgen resolves an endpoint:

| Mode | Behavior |
|---|---|
| `ServiceDefault` | Hit the target Service on its declared port. |
| `AutoDiscoverProbe` | Reuse the target's readiness probe path. |
| `CustomPath` | Drive a specific path set in `spec.target.customPath`. |

`spec.target.networkPath` isolates *where* a bottleneck lives:

- `ClusterIP`: hit the Service directly, in-cluster.
- `Ingress`: route through the edge gateway to include ingress overhead.

## Load

- `spec.load.baseRps`: steady-state requests per second.
- `spec.load.concurrencyFactor`: in-flight connection multiplier driving the ramp.

## Chaos disruption

Enabling `spec.disruption.injectPodDeletion` terminates one healthy replica at peak load to exercise `terminationGracePeriodSeconds`, `preStop` hooks, and EndpointSlice propagation. `minReplicasForChaos` is a safety floor: chaos is skipped below it. `triggerDelay` offsets the kill from the start of peak load.

## SLA

`spec.sla` is the HPA scale-up latency budget. The run's verdict band (pass / warn / fail) is computed against it. See [Observability](observability.md) for the emitted metrics.
