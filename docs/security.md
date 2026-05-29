# Security

## Verifying signatures

Every released image and the OCI chart are signed with [cosign](https://docs.sigstore.dev/) keyless signing. The signing identity is the GitHub Actions OIDC token bound to this repository's release workflow, so there are no keys to distribute. Verify provenance before installing:

```bash
cosign verify ghcr.io/ethan-kane-ops/scale-sentry:v0.1.1 \
  --certificate-identity-regexp 'https://github.com/ethan-kane-ops/scale-sentry/.+' \
  --certificate-oidc-issuer https://token.actions.githubusercontent.com
```

The same command verifies the loadgen and observer images, and the chart at `ghcr.io/ethan-kane-ops/charts/scale-sentry`.

## Reporting a vulnerability

Report vulnerabilities privately via [GitHub Security Advisory](https://github.com/ethan-kane-ops/scale-sentry/security/advisories/new). See [SECURITY.md](https://github.com/ethan-kane-ops/scale-sentry/blob/main/SECURITY.md) for the full disclosure policy.
