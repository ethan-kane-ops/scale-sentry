# Security Policy

## Reporting a Vulnerability

Please **do not** open a public GitHub issue for security-related reports.

Report vulnerabilities privately via the GitHub Security Advisory page for this repository:

https://github.com/ethan-kane-ops/scale-sentry/security/advisories/new

When submitting a report, please include:

- **Affected component**, controller, observer, loadgen, or chart.
- **Description**, what the vulnerability is and its potential impact.
- **Reproduction**, step-by-step instructions, including any sample `ScaleValidation` manifests or RBAC configurations.
- **Mitigation**, any temporary workarounds or fixes you have identified.

### Response targets

| Stage                                | Target  |
|--------------------------------------|---------|
| Acknowledge receipt                  | 48 hours |
| Triage and confirm severity          | 7 days  |
| Fix released for critical severity   | 14 days |

Reporters will be credited in the release notes for the fix unless they request anonymity.

## Signed Releases

From v0.1.1 onward, all published container images and the OCI Helm chart are
signed with [cosign](https://docs.sigstore.dev/) keyless signing. The signing
identity is the GitHub Actions OIDC token bound to this repository's release
workflow (no long-lived keys). Verify provenance with `cosign verify`, see the
[Install](./README.md#verifying-signatures) section of the README for the exact
command.

## Supported Versions

Only the latest minor release line receives security updates.

| Version | Supported |
|---------|-----------|
| v0.1.x  | Yes       |
| < v0.1  | No        |
