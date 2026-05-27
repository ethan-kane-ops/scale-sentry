# Contributing to Scale Sentry

Thank you for considering a contribution. This document describes the development workflow, coding standards, and pull request expectations for the project.

By participating, you agree to abide by the [Code of Conduct](./CODE_OF_CONDUCT.md).

---

## Development Setup

The project uses [mise](https://mise.jdx.dev/) to manage language runtimes and tooling, and [just](https://just.systems/) as a task runner. Both are cross-platform (macOS, Linux, Windows via WSL).

### Prerequisites

1. Install **mise**, follow the platform-specific instructions at https://mise.jdx.dev/installing-mise.html
2. Activate **mise** in your shell, see https://mise.jdx.dev/getting-started.html#activate-mise
3. Provision the project's pinned tools (Go, kubectl, helm, kind, just, kubeconform, golangci-lint):
   ```bash
   mise install
   ```
4. Install **pre-commit**, follow the platform-specific instructions at https://pre-commit.com/#install
5. Enable the hooks:
   ```bash
   pre-commit install
   ```

### Local cluster

For end-to-end iteration:

```bash
just dev-up     # creates a Kind cluster, builds images, installs the chart
just dev-down   # tears it down
```

---

## Common Targets

Run `just --list` for the full set. The most-used recipes:

| Target               | Purpose                                                |
|----------------------|--------------------------------------------------------|
| `just check`         | tidy + lint + unit tests, **required before every commit** |
| `just test`          | unit tests only                                        |
| `just test-integration` | envtest suite (downloads apiserver + etcd binaries on first run) |
| `just test-e2e`      | full verdict E2E inside a Kind cluster                 |
| `just lint`          | `go vet` + `golangci-lint` with `envtest` and `e2e` build tags |
| `just generate`      | regenerate `zz_generated.deepcopy.go`                  |
| `just manifests`     | regenerate CRD + RBAC YAML from kubebuilder markers    |

Run `just generate && just manifests` after any change to `api/v1alpha1/*_types.go`. Generated files are committed; CI fails on drift.

---

## Coding Guidelines

### Errors

- Lowercase strings, no trailing punctuation.
- Wrap with `%w` to preserve the cause: `fmt.Errorf("fetching endpoints: %w", err)`.

### Tests

- Table-driven for any function with more than two logical branches.
- Use `t.TempDir()` for filesystem fixtures.
- No `time.Sleep` waits, use `eventually` helpers, channels, or fake clocks.
- No network dependencies in unit tests, use `httptest.Server` or mocks.

### CRD types

- Any change to `api/v1alpha1/*_types.go` requires `just generate && just manifests`.
- `FailureRate` and any other `float64` field require `crd:allowDangerousTypes=true` on the controller-gen invocation (already wired in the justfile).

---

## Commit Message Format

The project uses [Conventional Commits](https://www.conventionalcommits.org/) to drive automated changelog generation via [git-cliff](https://git-cliff.org/).

Format:

```
<type>(<scope>): <description>

[optional body]

[optional footer(s)]
```

**Types**: `feat`, `fix`, `docs`, `style`, `refactor`, `perf`, `test`, `chore`, `ci`, `revert`.

**Examples**:

```
feat(controller): auto-discover container readiness probe paths
fix(observer): scrape cAdvisor through kubelet proxy, not pods/exec
perf(loadgen): replace unbounded slice with HDR histogram
```

Keep the summary under 72 characters and in imperative mood.

---

## Pull Request Workflow

1. Fork and create a branch from `main`:
   ```bash
   git checkout -b feat/short-description
   ```
2. Make focused commits, one logical change per commit, each independently buildable.
3. Run `just check` locally. For CRD or RBAC changes, also run `just test-integration`.
4. Push your branch and open a Pull Request against `main`. Fill out the PR template.
5. Pull Requests require CI to pass and at least one maintainer approval before merge.
