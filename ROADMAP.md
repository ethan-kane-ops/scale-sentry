# Roadmap

This is a statement of direction, not a promise of dates. Items move when evidence (users, failures, adoption) says they should. Issues are tracked on the [issue tracker](https://github.com/ethan-kane-ops/scale-sentry/issues).

## Delivered in v0.4.0

- **E2E scenario matrix**: gRPC, HTTP/2, Gateway, chaos-disruption, finalizer, and annotation-bridge scenarios running nightly in CI, with the race detector on the unit path.
- **Fuzzing**: native Go fuzz targets for the cgroup, report, and annotation parsers, on a weekly CI budget.
- **Day-two docs**: task guides (gRPC validation, Envoy Gateway, CI gating) and a troubleshooting page.
- **Visual polish**: run-lifecycle sequence diagram, README hero, reproducible demo recording.
- **Run history**: the last ten terminal verdicts are kept in `status.history`, so trend is
  visible from `kubectl get -o json` without a metrics stack. A one-shot validation fills one
  entry; `spec.schedule` (below) is what accumulates the rest.

## Next (unreleased)

- **Recurring validation**: `spec.schedule` re-runs a validation on a cron expression and
  `spec.suspend` pauses it, so `status.history` and the run metrics carry a trend rather than
  a single data point. Runs never overlap.

## API graduation: v1alpha1 to v1beta1

`ScaleValidation` graduates to `v1beta1` when all of the following hold:

1. No breaking field changes across two consecutive minor releases.
2. The nightly e2e scenario matrix has been green for a sustained period (no flaky-quarantine entries).
3. At least one adopter running validations we did not write ourselves.
4. A served-and-deprecated conversion story for `v1alpha1` is decided and tested.

Until then `v1alpha1` may change between minors; changes are called out in release notes.

## Parked, with revival conditions

- **ValidatingAdmissionWebhook**: parked. CRD enum + structural validation catches malformed specs at apply time, and lifecycle Events narrate runtime failures. Revives if users report that apply-time feedback is insufficient in practice.
- **ScalingBaseline CRD** (store expected-good verdicts and diff against them): parked as speculative until real users articulate the workflow.
- **Static `install.yaml` release asset**: the Helm chart is the supported install path. Revives on demand from non-Helm users.

## Kubernetes version support

Kubernetes 1.34 is the supported floor. The nightly envtest matrix runs the controller suite against 1.35 and 1.34, and the kind-based e2e suite runs against the version pinned in CI. Minors older than 1.34 may work but are not verified.
