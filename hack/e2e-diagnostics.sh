#!/usr/bin/env bash
# Best-effort forensic dump for a failed e2e run, since the kind cluster
# dies with the runner and CI has no interactive access to it. Written to
# $OUT_DIR for the workflow to upload as an artifact.
#
# No `set -e`: a single missing resource (e.g. a namespace a failed test
# already cleaned up) must not cut the dump short.
set -uo pipefail

OUT_DIR="${1:-/tmp/e2e-diagnostics}"
mkdir -p "$OUT_DIR"

kubectl get pods -A -o wide >"$OUT_DIR/pods.txt" 2>&1
kubectl get scalevalidations -A -o yaml >"$OUT_DIR/scalevalidations.yaml" 2>&1
kubectl get hpa -A -o wide >"$OUT_DIR/hpa.txt" 2>&1
kubectl describe hpa -A >"$OUT_DIR/hpa-describe.txt" 2>&1
kubectl get events -A --sort-by=.lastTimestamp >"$OUT_DIR/events.txt" 2>&1

kubectl logs -n default deployment/scale-sentry-controller --all-containers --tail=-1 \
    >"$OUT_DIR/controller.log" 2>&1
kubectl logs -n kube-system deployment/metrics-server --tail=-1 \
    >"$OUT_DIR/metrics-server.log" 2>&1

# loadgen/observer run as per-ScaleValidation Jobs inside whichever
# namespace the scenario created (scale-sentry-e2e, generated ss-e2e-chaos-*,
# ...), so grab every pod's logs in every namespace that isn't kube-system.
for ns in $(kubectl get ns -o jsonpath='{.items[*].metadata.name}'); do
    case "$ns" in
    kube-system | kube-node-lease | kube-public | local-path-storage) continue ;;
    esac
    for pod in $(kubectl get pods -n "$ns" -o jsonpath='{.items[*].metadata.name}' 2>/dev/null); do
        kubectl logs -n "$ns" "$pod" --all-containers --prefix --tail=-1 \
            >>"$OUT_DIR/workload-pods.log" 2>&1
    done
done

echo "diagnostics written to $OUT_DIR"
