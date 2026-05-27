## Description

A summary of the change and the issue(s) it addresses, including relevant motivation, context, and any dependencies.

Fixes # (issue reference)

## Type of Change

- [ ] **Bug fix**, non-breaking change which fixes an issue
- [ ] **New feature**, non-breaking change which adds functionality
- [ ] **Breaking change**, alters behavior such that existing users would need to update
- [ ] **Documentation**, additions or changes to docs, examples, or schemas
- [ ] **Refactor**, restructuring or performance work with no behavior change

## Quality Checklist

- [ ] `just check` passes locally (tidy + lint + unit tests).
- [ ] Tests added or updated to cover the change (unit and/or `envtest`).
- [ ] For CRD changes: `just generate && just manifests` was run, and the generated files are committed.
- [ ] For CRD or RBAC changes: `just test-integration` was run locally.
- [ ] Documentation updated where behavior, configuration, or public API changed.
- [ ] Commit messages follow [Conventional Commits](https://www.conventionalcommits.org/) (e.g. `feat(controller): ...`).
