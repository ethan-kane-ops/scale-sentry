# scale-sentry end-to-end fixtures

A self-contained set of test workloads and `ScaleValidation` CRs that exercise the operator across the protocol x network-path matrix:

| Protocol | ClusterIP                         | Gateway (Envoy)               |
|----------|-----------------------------------|-------------------------------|
| HTTP/1.1 | `e2e-http1-clusterip`             | `e2e-http1-gateway`           |
| HTTP/2   | `e2e-http2-clusterip` (h2c)       | `e2e-http2-gateway` (h2c)     |
| gRPC     | `e2e-grpc-clusterip`              | `e2e-grpc-gateway`            |

All six target workloads are CPU-bound with HPA `min=1, max=10, target=70% CPU`, so a sustained load run will trigger scale-up while scale-sentry watches.

## Prereqs

1. A Kubernetes cluster with the metrics-server installed (HPAs need it).
2. The Gateway API CRDs (`gateway.networking.k8s.io/v1`):

   ```bash
   kubectl apply -f https://github.com/kubernetes-sigs/gateway-api/releases/download/v1.2.0/standard-install.yaml
   ```

3. Envoy Gateway (recommended modern Ingress; classic `kubernetes/ingress-nginx` is sunset):

   ```bash
   helm install eg oci://docker.io/envoyproxy/gateway-helm \
       --version v1.2.0 \
       --namespace envoy-gateway-system --create-namespace
   ```

4. The scale-sentry operator installed in the cluster via the Helm chart (see the [install docs](https://ethan-kane-ops.github.io/scale-sentry/getting-started/)).

## Install the fixtures

```bash
kubectl apply -f config/e2e/00-namespace.yaml
kubectl apply -f config/e2e/targets/
kubectl apply -f config/e2e/envoy-gateway/
```

Wait for the workloads to be Ready and Envoy Gateway to provision the listener Service:

```bash
kubectl -n scale-sentry-e2e get deploy,svc,hpa
kubectl -n envoy-gateway-system get svc -l gateway.envoyproxy.io/owning-gateway-name=scale-sentry-eg
```

The Service name in the second command is what the `Gateway`-path validations target via `spec.target.host`. The default (`envoy-scale-sentry-e2e-scale-sentry-eg.envoy-gateway-system.svc.cluster.local`) matches the upstream Envoy Gateway naming scheme; if your install names the Service differently, edit the four `*-gateway.yaml` CRs accordingly.

## Run a validation

```bash
# Pick one of the six:
kubectl apply -f config/e2e/validations/http2-clusterip.yaml

# Watch the run:
kubectl -n scale-sentry-e2e get scalevalidation,jobs,pods
kubectl -n scale-sentry-e2e logs -l validation.scale-sentry.ek.co/loadgen-for=e2e-http2-clusterip -c loadgen -f
```

Run-summary JSON lands in the loadgen pod logs and the observer's report once the SLA window closes. HPA reaction time is reported in `.status.scaleUpDuration`.

## Tear down

```bash
kubectl delete ns scale-sentry-e2e
```

The Gateway, Routes, Deployments, Services, HPAs, and ScaleValidations all live in `scale-sentry-e2e`, so a single namespace delete reclaims everything.
