# Test Through Envoy Gateway

`networkPath: ClusterIP` measures pure scaling behavior. Real users arrive through the edge, and the edge has its own failure modes: listener capacity, route configuration, and connection reuse that changes how load distributes across pods. Running the same validation through both paths tells you which layer owns a problem.

## Prerequisites

Gateway API CRDs and Envoy Gateway:

```bash
kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.0/standard-install.yaml
helm install eg oci://docker.io/envoyproxy/gateway-helm \
  --version v1.2.0 \
  --namespace envoy-gateway-system --create-namespace
```

The repo ships a complete fixture set (Gateway, HTTPRoutes/GRPCRoutes, CPU-bound targets with HPAs):

```bash
kubectl apply -f https://raw.githubusercontent.com/ethan-kane-ops/scale-sentry/main/config/e2e/00-namespace.yaml
kubectl apply -f https://raw.githubusercontent.com/ethan-kane-ops/scale-sentry/main/config/e2e/targets/whoami.yaml
kubectl apply -f https://raw.githubusercontent.com/ethan-kane-ops/scale-sentry/main/config/e2e/envoy-gateway/gateway.yaml
kubectl apply -f https://raw.githubusercontent.com/ethan-kane-ops/scale-sentry/main/config/e2e/envoy-gateway/routes.yaml
```

## The two Gateway-specific knobs

```yaml
spec:
  target:
    mode: ServiceDefault
    port: 80
    networkPath: Gateway
    host: envoy-scale-sentry-e2e-scale-sentry-eg.envoy-gateway-system.svc.cluster.local
```

- `networkPath: Gateway` routes loadgen traffic through the edge instead of the target Service.
- `host` points the loadgen at the Envoy listener Service. Envoy Gateway names it `envoy-<gateway-namespace>-<gateway-name>` in `envoy-gateway-system`; find yours with:

```bash
kubectl -n envoy-gateway-system get svc \
  -l gateway.envoyproxy.io/owning-gateway-name=scale-sentry-eg
```

`host` also sets the Host header, which is how the HTTPRoute picks the backend. A wrong `host` shows up as 404s (route mismatch) rather than connection errors.

## Interpret the delta

Run the same spec twice, once per `networkPath`, and compare:

| Observation | Meaning |
|---|---|
| Both pass | Scaling and edge are both healthy at this rate |
| ClusterIP passes, Gateway fails | The edge owns the problem: listener limits, route config, or Envoy pod resources (it has its own HPA story) |
| Both fail with similar latency curves | The workload itself is the bottleneck; fix the target or its HPA before touching the edge |
| Gateway p99 much worse under HTTP/2 | Multiplexing concentrates streams onto fewer upstream connections; check Envoy's upstream connection-pool settings |

Ready-made CR pairs for the comparison live in [`config/e2e/validations/`](https://github.com/ethan-kane-ops/scale-sentry/tree/main/config/e2e/validations), covering h1/h2/gRPC through both paths. The classic `Ingress` value remains supported for legacy clusters; new setups should prefer Gateway API.
