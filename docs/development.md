# Development

## Prerequisites

- Go >= 1.25 (toolchain pin in `go.mod`)
- [mise](https://mise.jdx.dev/installing-mise.html), runtime + tool manager
- [just](https://just.systems/man/en/), task runner (provisioned by `mise install`)
- A local Kubernetes cluster: [Kind](https://kind.sigs.k8s.io/) or [Minikube](https://minikube.sigs.k8s.io/)

## Bring up a dev cluster

```bash
mise install              # provisions Go, kubectl, helm, kind, just, kubeconform
just dev-up               # creates Kind cluster, builds + loads images, installs the chart
```

Apply a sample CR:

```bash
kubectl apply -f config/samples/targets/podinfo.yaml
kubectl apply -f config/samples/scalevalidation-servicedefault.yaml
```

Tear down:

```bash
just dev-down
```

## Common tasks

| Command | Purpose |
|---|---|
| `just check` | tidy + lint + unit tests, required before every commit |
| `just test-integration` | envtest suite (downloads apiserver + etcd assets) |
| `just test-e2e` | full verdict E2E in Kind |
| `just generate` | regenerate `zz_generated.deepcopy.go` |
| `just manifests` | regenerate CRD + RBAC YAML from kubebuilder markers |
| `just docs-serve` | generate the API reference and serve the docs site locally |

Run `just generate && just manifests` after any change to `api/v1alpha1/*_types.go`.

## Documentation

The docs site is [MkDocs Material](https://squidfunk.github.io/mkdocs-material/). The API reference page is generated from the kubebuilder markers by [`crd-ref-docs`](https://github.com/elastic/crd-ref-docs) and is not committed (it is regenerated on every build).

```bash
just docs-api      # regenerate docs/reference/api.md
just docs-serve    # generate API ref + live-reload server on :8000
just docs-build    # generate API ref + build static site into ./site
```

See [Contributing](https://github.com/ethan-kane-ops/scale-sentry/blob/main/CONTRIBUTING.md) for the pull request workflow.
