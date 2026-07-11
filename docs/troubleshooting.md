# Troubleshooting

The fastest diagnostic is always the Event stream: the controller narrates every state transition with one of [nine stable reasons](events.md).

```bash
kubectl describe scalevalidation <name>          # Events section at the bottom
```

## Run goes to `Error` with `TargetReadyTimeout`

The target Deployment never reached ready replicas within the SLA plus grace window. The loadgen is deliberately never started against an unready target (it would poison the verdict). Check the target itself:

```bash
kubectl get deploy <target> -o wide
kubectl describe pod -l app=<target>   # image pulls, probe failures, scheduling
```

## HPA never scales, `slaStatus: Fail` with no `scaleUpDuration`

Almost always metrics-server. HPAs cannot act without it:

```bash
kubectl top nodes                        # errors if metrics-server is absent
kubectl get hpa <target> -o wide         # TARGETS column shows <unknown>
```

On kind and most local clusters, install metrics-server with the kubelet TLS patch:

```bash
kubectl apply -f https://github.com/kubernetes-sigs/metrics-server/releases/latest/download/components.yaml
kubectl -n kube-system patch deploy metrics-server --type=json \
  -p='[{"op":"add","path":"/spec/template/spec/containers/0/args/-","value":"--kubelet-insecure-tls"}]'
```

Also confirm the target actually has an HPA: scale-sentry validates HPA behavior, it does not create one for you.

## Run goes to `Error` with `LoadgenJobFailed`

The loadgen pod exited non-zero and produced no usable result. Read its logs before it is garbage-collected:

```bash
kubectl logs job/<loadgen-job-name> -c loadgen
```

Common causes:

- **Protocol mismatch**: `protocol: GRPC` against a plain HTTP server (Health/Check RPCs fail immediately), or `HTTP2` h2c against a server that only speaks HTTP/1.1.
- **Connection refused**: wrong `port`, or a `CustomPath` that the server rejects.
- **Gateway 404s**: `host` does not match any route; see [Test Through Envoy Gateway](guides/envoy-gateway.md).

## Run goes to `Error` with `TLSCABundleMissing`

`spec.target.tls.caBundle.configMapRef` points at a ConfigMap or key that does not exist in the CR's namespace. The Event message names exactly which. Create the ConfigMap and re-apply the CR.

## Jobs rejected in PSA `restricted` namespaces

The loadgen and observer Jobs ship Pod Security Admission Restricted-compliant security contexts (non-root, seccomp, dropped capabilities), so `restricted`-enforcing namespaces are supported on chart >= 0.2.1. If a Job is still rejected, check for namespace-level policy engines (Kyverno, Gatekeeper) with rules beyond PSA, and read the Job's `kubectl describe` for the exact admission message.

## Verdict is `Unknown`

`slaStatus` and `trafficIntegrity` stay `Unknown` when the run terminated without a usable observer verdict: the loadgen failed, the target never became ready, or TLS material was missing. In each case the phase is `Error` and the Event reason identifies the cause. An `Unknown` verdict is never a pass; treat it as a failed gate.

## Latency numbers look implausible

Check `status.diagnostics` for `MetricsLikelySkewed`: it fires when cold-start leakage or drain errors landed inside the measurement window, meaning the histogram contains error-path responses. Fix the leakage or drain finding first, then re-run for clean latency data. Also confirm you set `warmupDuration`: without it, TCP/TLS handshakes and cold caches pollute the first seconds ([Load Profiles](load-profiles.md)).

## Still stuck

Open a [GitHub Discussion or issue](https://github.com/ethan-kane-ops/scale-sentry/issues) with the CR spec, the Events output, and the loadgen/observer logs.
