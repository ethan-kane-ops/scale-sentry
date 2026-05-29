# Scale Sentry

**Scale Sentry** validates Kubernetes auto-scaling behavior under load. It generates dynamic traffic to a target Deployment, tracks HPA scale-up latency against an SLA, and correlates HTTP errors with EndpointSlice updates to surface **cold-start traffic leakage**: errors served the instant a new pod is declared Ready.

It is a [kubebuilder](https://book.kubebuilder.io/) v4 controller built on `controller-runtime`. Workloads are validated declaratively through a `ScaleValidation` Custom Resource or through annotations on existing Deployments.

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

## Features

- **Custom Resource driven**: the `validation.scale-sentry.ek.co/v1alpha1` `ScaleValidation` resource stores test configuration, SLA targets, and execution history in the resource's `status` subresource.
- **Annotation bridge**: annotating an existing `Deployment` with `validation.scale-sentry.ek.co/enabled=true` provisions a shadow `ScaleValidation` automatically, no manifests required.
- **Endpoint targeting modes**: three target resolution strategies (`ServiceDefault`, `AutoDiscoverProbe`, `CustomPath`) and two network paths (direct `ClusterIP` or `Ingress` gateway) to isolate scaling bottlenecks from edge bottlenecks.
- **Chaos disruption**: optionally terminates a healthy replica at peak load to test `terminationGracePeriodSeconds`, `preStop` hooks, and EndpointSlice propagation delays.
- **Diagnostic suite**:
    - **Readiness lag analyzer**: measures `PodRunning` to `PodReady` delta to detect sparse probe sampling.
    - **TCP / TLS handshake tester**: short-lived versus persistent connection pools.
    - **cgroup throttle watcher**: scrapes `nr_throttled` / `nr_periods` via Kubelet cAdvisor to flag CFS quota throttling.
    - **DNS + PDB auditor**: flags `ndots:5` resolver pressure and missing `PodDisruptionBudget`s.

## Next steps

- [Getting Started](getting-started.md): install the chart and run a first validation.
- [Configuration](configuration.md): the full `ScaleValidation` spec.
- [API Reference](reference/api.md): generated CRD field reference.
