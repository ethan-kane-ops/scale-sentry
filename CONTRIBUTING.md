# Contributing to Scale Sentry

First off, thank you for taking the time to contribute! Contributions from the community help make Scale Sentry a robust and reliable tool for everyone.

This document provides a set of guidelines for contributing to this repository.

---

## Code of Conduct

By participating in this project, you agree to maintain a respectful, welcoming, and professional environment. Please be kind and constructive in your feedback and reviews.

---

## Development Setup

This project uses [mise](https://mise.jdx.dev/) to manage runtime dependencies and [just](https://just.systems/) as a task runner.

### Prerequisites

1. Install **mise**:
   ```bash
   brew install mise
   ```
2. Enable mise in your shell:
   ```bash
   mise activate
   ```
3. Install dependencies (Go, linter, task tools):
   ```bash
   mise install
   ```

### Common Targets

Use `just` to coordinate your work:
* `just build` — Compile the Go binary to `bin/scale-sentry`.
* `just test` — Run all unit tests.
* `just lint` — Run `golangci-lint` check.
* `just check` — Tidy dependencies, lint, and run tests (run this before pushing!).

---

## Coding Guidelines

To maintain code quality, please adhere to the following Go paradigms:

1. **Error Formatting:**
   * Standard library style: lower-case, no trailing punctuation.
   * Wrap errors with `%w` for context propagation: `fmt.Errorf("fetching endpoints: %w", err)`.
2. **Testing Standards:**
   * Write **table-driven tests** for complex logical paths.
   * Use `t.TempDir()` for temporary directory fixtures.
   * Avoid long sleeps or external network dependencies in unit tests. Mock external calls or use test servers.
3. **Pre-commit Hooks:**
   * Set up pre-commit hooks:
     ```bash
     pre-commit install
     ```
   * Hooks run automatically on commit to verify formatting and lint rules.

---

## Commit Message Guidelines

We enforce **Conventional Commits** to automate changelog generation and versioning. Ensure your commit message follows this format:

```
<type>(<scope>): <description>

[optional body]

[optional footer(s)]
```

### Types:
* `feat`: A new user-facing feature.
* `fix`: A bug fix.
* `docs`: Documentation changes.
* `style`: Code formatting changes (missing semi-colons, white space, etc.).
* `refactor`: Code changes that neither fix a bug nor add a feature.
* `perf`: Performance improvements.
* `test`: Adding or correcting tests.
* `chore`: Maintenance tasks, dependencies, build configs.

### Example:
```
feat(controller): add auto-discovery for container readiness probes
```

---

## Pull Request Workflow

1. Fork the repository and create your branch from `main`:
   ```bash
   git checkout -b feat/my-new-feature
   ```
2. Implement your changes and add corresponding tests.
3. Run verification checks:
   ```bash
   just check
   ```
4. Commit your changes following Conventional Commit conventions.
5. Push to your fork and submit a Pull Request targeting `main`.
