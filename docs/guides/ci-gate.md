# Gate a Pipeline on a Verdict

A `ScaleValidation` is declarative and its verdict lands on `status`, which makes it a natural CD gate: deploy a canary, validate its scaling behavior under synthetic load, and only promote if the verdict passes.

## The core recipe

```bash
kubectl apply -f scalevalidation.yaml

if kubectl wait scalevalidation/payment-canary -n staging \
    --for=jsonpath='{.status.phase}'=Succeeded --timeout=15m; then
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

- `phase: Succeeded` is the terminal success state and implies the SLA and traffic-integrity checks passed. `Failed` and `Error` are the terminal non-success states; `kubectl wait` times out on them, which the `if` turns into a pipeline failure with the Events dump explaining why.
- Size `--timeout` as `spec.sla` + warmup + load duration + scheduling slack. A 90s SLA run typically completes in a few minutes; 15m is a generous ceiling.
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
          --for=jsonpath='{.status.phase}'=Succeeded --timeout=15m
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

For scheduled (non-pipeline) validations, alert on the metrics instead of polling status: `scale_sentry_runs_total{verdict="fail"}` and the `VerdictFail` Event reason are both stable interfaces. See [Observability](../observability.md) and [Events](../events.md).
