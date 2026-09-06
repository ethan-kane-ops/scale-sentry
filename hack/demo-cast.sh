#!/usr/bin/env bash
# The scenario behind the asciinema recording on ethankane.net.
#
# hack/demo.sh already records the README GIF. This is a second script rather
# than a flag on that one because the two want different things: the GIF is a
# quickstart and stops at "it worked", while the site recording exists to show
# the verdict — a measured scale-up latency judged against a declared SLA — and
# so it leads with the HPA that has never been tested and ends on the number.
#
#   just cast-setup   # cluster, operator, metrics-server, target — not recorded
#   just cast         # this script, under asciinema rec
#
# The run is real and takes minutes. asciinema records that honestly and
# --idle-time-limit trims the dead air on playback without touching the
# timestamps.
set -euo pipefail

VALIDATION="podinfo-default"
TARGET="podinfo"

# The recording is published, so refuse anything that is not the local kind
# cluster. Checked before a single frame exists.
ctx=$(kubectl config current-context)
case "$ctx" in
  kind-*) ;;
  *) echo "refusing to record against non-kind context: $ctx" >&2; exit 1 ;;
esac

type_line() {
  local line="$1" i
  printf '\033[38;5;108m$\033[0m '
  for ((i = 0; i < ${#line}; i++)); do
    printf '%s' "${line:i:1}"
    sleep 0.022
  done
  printf '\n'
}

# The command shown is the command that runs.
run() {
  type_line "$1"
  eval "$1"
  echo
}

note() {
  printf '\033[38;5;245m# %s\033[0m\n' "$1"
  sleep 1.4
}

WATCH_PID=""
cleanup() { [[ -n "$WATCH_PID" ]] && kill "$WATCH_PID" 2>/dev/null || true; }
trap cleanup EXIT

clear

note "An HPA that has never been tested under load."
echo

run "kubectl get deploy $TARGET && kubectl get hpa $TARGET"

note "One replica, and a scaling policy nobody has proven."
note "The claim to check: it can absorb a spike inside 90 seconds."
echo

run "kubectl apply -f config/samples/scalevalidation-servicedefault.yaml"

note "The operator now drives real traffic at the Service and watches"
note "the replica count climb. This is a live run, not a simulation."
echo

# The watch is the interesting part: the phases are real transitions, not a
# progress bar this script invented.
type_line "kubectl get scalevalidation $VALIDATION -w"
kubectl get scalevalidation "$VALIDATION" -w &
WATCH_PID=$!
kubectl wait "scalevalidation/$VALIDATION" \
  --for=jsonpath='{.status.phase}'=Succeeded --timeout=600s >/dev/null
sleep 2
kill "$WATCH_PID" 2>/dev/null && WATCH_PID=""
echo

note "The target scaled while the load was running."
echo

run "kubectl get hpa $TARGET && kubectl get pods -l app=$TARGET"

note "And the verdict, with the number it was judged on."
echo

run "kubectl get scalevalidation $VALIDATION -o custom-columns='SLA:.spec.sla,MEASURED:.status.scaleUpDuration,VERDICT:.status.slaStatus,REQUESTS:.status.totalRequests'"

note "Measured, not asserted. The HPA is now a tested claim."
echo

# The run also reports what it noticed on the way past. These are real findings
# about the target, not a fixed checklist: this cluster genuinely has no PDB.
note "It also reports what it saw while it was in there."
echo

run "kubectl get scalevalidation $VALIDATION -o jsonpath='{range .status.diagnostics[*]}{.severity}{\"  \"}{.type}{\"\\n\"}{end}'"

sleep 2
