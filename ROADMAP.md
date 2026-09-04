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

## Delivered in v0.5.0

- **`v1beta1`**: the API graduated. See below for what changed and what it commits us to.
- **Recurring validation**: `spec.schedule` re-runs a validation on a cron expression and
  `spec.suspend` pauses it, so `status.history` and the run metrics carry a trend rather than
  a single data point. Runs never overlap.
- **Spec edits take effect**: editing a finished validation starts a new run, tracked with
  `status.observedGeneration`, instead of leaving a result that describes a spec the object
  no longer carries.
- **Cross-namespace observer RBAC**: `observer.namespaces` installs the observer's Role into
  every namespace that runs validations, rather than only the chart's release namespace.

## API version: v1beta1

`ScaleValidation` is served at `validation.scale-sentry.ek.co/v1beta1`. **`v1alpha1` is gone, not deprecated.** It is no longer served, there is no conversion webhook, and a `v1alpha1` object will be rejected by the apiserver. Recreate any that exist.

That is a hard break, taken deliberately while the project had no external consumers. It will not be repeated: from here, breaking changes to `v1beta1` require a served-and-deprecated migration path.

### What changed in the graduation

- **No floats.** `status.failureRate` became `status.failureRateBasisPoints`, an integer where 1 bp is 0.01%, so the 1% traffic-integrity threshold is exactly 100. The Kubernetes API convention rejects floats because they do not round-trip reliably across clients, and the CRD no longer needs `controller-gen crd:allowDangerousTypes`.
- **Named types for every closed value set.** `phase`, `target.mode`, `networkPath`, `protocol`, `profile.pattern`, diagnostic `severity`, and the verdict fields are now distinct Go types with exported constants. The wire format is unchanged; the benefit is that the controller can no longer compare a status field against a bare string literal and get away with a typo.
- **`spec.suspend` stays a plain `bool`.** There is no meaningful difference here between unset and false, so a pointer would add nil-handling for nothing. CronJob's `*bool` is historical.

### What v1beta1 commits us to

Field removals and semantic changes now need a deprecation cycle: served alongside the replacement for at least one minor, called out in release notes, and removed no earlier than the following minor. Additive fields remain fair game at any time.

## Parked, with revival conditions

- **ValidatingAdmissionWebhook**: parked. CRD enum + structural validation catches malformed specs at apply time, and lifecycle Events narrate runtime failures. Revives if users report that apply-time feedback is insufficient in practice.
- **ScalingBaseline CRD** (store expected-good verdicts and diff against them): parked as speculative until real users articulate the workflow.
- **Static `install.yaml` release asset**: the Helm chart is the supported install path. Revives on demand from non-Helm users.

## Kubernetes version support

Kubernetes 1.34 is the supported floor. The nightly envtest matrix runs the controller suite against 1.35 and 1.34, and the kind-based e2e suite runs against the version pinned in CI. Minors older than 1.34 may work but are not verified.
