# Gate a Pipeline on a Verdict

A `ScaleValidation` is declarative and its verdict lands on `status`, which makes it a natural CD gate: deploy a canary, validate its scaling behavior under synthetic load, and only promote if the verdict passes.

## The core recipe

```bash
kubectl apply -f scalevalidation.yaml

kubectl wait scalevalidation/payment-canary -n staging \
    --for=condition=Finished --timeout=15m

phase=$(kubectl get scalevalidation payment-canary -n staging -o jsonpath='{.status.phase}')
if [ "$phase" = "Succeeded" ]; then
  echo "scale validation passed"
else
  echo "scale validation did not pass:"
  kubectl get scalevalidation payment-canary -n staging \
    -o jsonpath='{.status.phase} sla={.status.slaStatus} traffic={.status.trafficIntegrity}{"\n"}'
  kubectl describe scalevalidation payment-canary -n staging | sed -n '/Events:/,$p'
  exit 1
fi
```

Notes on the mechanics:

- `Finished` goes True as soon as the run reaches any terminal phase, pass or fail. Waiting on it means a losing run fails the pipeline within seconds of the verdict instead of burning the whole `--timeout`.
- `Finished` is not a pass/fail signal, deliberately. `kubectl wait --for=condition=X` only ever waits for `X` to become True, so a condition that went False on failure would leave the gate blocking anyway. The verdict lives in `status.phase`: `Succeeded`, `Failed`, or `Error`.
- The condition's `reason` says why the run ended, and is a stable string worth matching on: `Succeeded`, `VerdictFailed`, `TargetNotReady`, `TargetUnsupported`, `TLSCABundleMissing`, `LoadgenJobFailed`, `LoadgenJobVanished`, `ObserverReportUnreadable`, `TargetURLUnresolved`, `JobBuildFailed`.

  ```bash
  kubectl get scalevalidation payment-canary -n staging \
    -o jsonpath='{.status.conditions[?(@.type=="Finished")].reason}{"\n"}'
  ```

- `kubectl wait` still times out if the controller never reaches a verdict at all (a stuck target, a controller that is down). Size `--timeout` as `spec.sla` + warmup + load duration + scheduling slack. A 90s SLA run typically completes in a few minutes; 15m is a generous ceiling.
- Clean up with `kubectl delete scalevalidation payment-canary`. Deleting mid-run is safe: a finalizer tears down the loadgen and observer Jobs.

## GitHub Actions example

```yaml
scale-validation:
  runs-on: ubuntu-latest
  steps:
    - uses: actions/checkout@v4
    - uses: azure/setup-kubectl@v4
    - name: Authenticate to the cluster
      run: echo "${{ secrets.KUBECONFIG_B64 }}" | base64 -d > "$RUNNER_TEMP/kubeconfig"
      env:
        KUBECONFIG: ${{ runner.temp }}/kubeconfig
    - name: Run scale validation
      env:
        KUBECONFIG: ${{ runner.temp }}/kubeconfig
      run: |
        kubectl apply -f deploy/scalevalidation-canary.yaml
        kubectl wait scalevalidation/payment-canary -n staging \
          --for=condition=Finished --timeout=15m
        test "$(kubectl get scalevalidation payment-canary -n staging \
          -o jsonpath='{.status.phase}')" = Succeeded
    - name: Explain failure
      if: failure()
      env:
        KUBECONFIG: ${{ runner.temp }}/kubeconfig
      run: |
        kubectl describe scalevalidation payment-canary -n staging | sed -n '/Events:/,$p'
    - name: Clean up
      if: always()
      env:
        KUBECONFIG: ${{ runner.temp }}/kubeconfig
      run: kubectl delete scalevalidation payment-canary -n staging --ignore-not-found
```

Prefer short-lived OIDC-federated credentials over a long-lived kubeconfig secret where your cluster supports it.

## Alerting on verdicts outside CI

For scheduled (non-pipeline) validations, set [`spec.schedule`](../configuration.md#scheduling) and alert on the metrics instead of polling status: `scale_sentry_runs_total{verdict="fail"}` and the `VerdictFail` Event reason are both stable interfaces. A scheduled CR also keeps its last ten verdicts in `status.history`, so a trend is readable without a metrics stack. See [Observability](../observability.md) and [Events](../events.md).
