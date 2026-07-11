#!/usr/bin/env bash
# Reproducible quickstart demo, built for recording the README GIF.
#
#   record:  asciinema rec demo.cast -c ./hack/demo.sh
#   render:  agg --speed 2 --idle-time-limit 3 demo.cast docs/assets/demo.gif
#
# Assumes `just dev-up` has already provisioned the kind cluster and the
# operator. The HPA wait is real (minutes); agg's --idle-time-limit trims it.
set -euo pipefail

PROMPT='$ '
WATCH_PID=""
cleanup() { [[ -n "$WATCH_PID" ]] && kill "$WATCH_PID" 2>/dev/null || true; }
trap cleanup EXIT

run() {
    printf '%s%s\n' "$PROMPT" "$*"
    sleep 1
    "$@"
    sleep 2
}

run kubectl apply -f config/samples/targets/podinfo.yaml
run kubectl apply -f config/samples/scalevalidation-servicedefault.yaml

printf '%skubectl get scalevalidation podinfo-default -w\n' "$PROMPT"
kubectl get scalevalidation podinfo-default -w &
WATCH_PID=$!
kubectl wait scalevalidation/podinfo-default \
    --for=jsonpath='{.status.phase}'=Succeeded --timeout=600s >/dev/null
sleep 2
kill "$WATCH_PID" 2>/dev/null && WATCH_PID=""

printf '\n%skubectl describe scalevalidation podinfo-default\n' "$PROMPT"
kubectl describe scalevalidation podinfo-default | sed -n '/Events:/,$p'
sleep 3
