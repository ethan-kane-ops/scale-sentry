# Version pins, keep in sync with go.mod
controller_gen_version := "v0.16.5"

# Container image tag for local builds (dev workflow + E2E)
image_tag := "dev"

# Kind cluster name used by test-e2e
kind_cluster := "scale-sentry-e2e"

default:
    @just --list

# Build all binaries into ./bin/ (isolated, does not affect the installed binary)
build:
    go build -o bin/ ./cmd/...

# Install controller-gen + setup-envtest at pinned versions into $GOBIN
tools:
    #!/usr/bin/env bash
    set -euo pipefail
    go install sigs.k8s.io/controller-tools/cmd/controller-gen@{{controller_gen_version}}
    go install sigs.k8s.io/controller-runtime/tools/setup-envtest@latest
    echo "✅ tools installed"

# Generate CRD + RBAC manifests from kubebuilder markers.
# allowDangerousTypes=true permits float64 in status.failureRate (ratio).
manifests: tools
    controller-gen rbac:roleName=manager-role crd:allowDangerousTypes=true paths="./..." \
        output:crd:artifacts:config=config/crd/bases \
        output:rbac:artifacts:config=config/rbac

# Generate DeepCopy implementations (zz_generated.deepcopy.go)
generate: tools
    controller-gen object:headerFile="hack/boilerplate.go.txt" paths="./..."

# Run the locally built binary, safe during development, never touches the installed version
run *args: build
    ./bin/scale-sentry {{args}}

# Run all tests
test:
    go test ./...

# Run tests with race detector
test-race:
    go test -race ./...

# Run the envtest integration suite (downloads apiserver/etcd assets on first run)
test-integration: tools
    #!/usr/bin/env bash
    set -euo pipefail
    export KUBEBUILDER_ASSETS="$(setup-envtest use -p path)"
    go test -tags envtest ./internal/controller/...

# Run linters (envtest + e2e tags included so those suites are checked)
lint:
    go vet -tags envtest,e2e ./...
    golangci-lint run --build-tags envtest,e2e

# Tidy go modules
tidy:
    go mod tidy

# Tidy + lint + test
check: tidy lint test

# Remove build artifacts
clean:
    rm -rf bin/

# Build all three container images (controller, loadgen, observer) tagged :{{image_tag}}.
# DOCKER_BUILDKIT=1 is required because the Dockerfile uses $BUILDPLATFORM, a
# BuildKit-only variable. Colima without the buildx plugin installed falls
# back to the legacy builder otherwise, which leaves $BUILDPLATFORM empty and
# fails platform parsing.
docker-build:
    DOCKER_BUILDKIT=1 docker build --build-arg CMD=scale-sentry -t scale-sentry:{{image_tag}} .
    DOCKER_BUILDKIT=1 docker build --build-arg CMD=loadgen      -t scale-sentry-loadgen:{{image_tag}} .
    DOCKER_BUILDKIT=1 docker build --build-arg CMD=observer     -t scale-sentry-observer:{{image_tag}} .

# Package the Helm chart into ./dist/scale-sentry-*.tgz
helm-package:
    mkdir -p dist
    helm package charts/scale-sentry --destination dist

# Helm upgrade-install the chart against the current kubeconfig context.
# Overrides controller.image.repository to the local (un-prefixed) name because
# `just docker-build` tags images as `scale-sentry:<tag>`, while the chart's
# default repository is `ghcr.io/ethan-kane-ops/scale-sentry` for releases.
deploy:
    helm upgrade --install scale-sentry charts/scale-sentry \
        --set controller.image.repository=scale-sentry \
        --set controller.image.tag={{image_tag}} \
        --set loadgenImage=scale-sentry-loadgen:{{image_tag}} \
        --set observerImage=scale-sentry-observer:{{image_tag}}

# Helm uninstall the chart from the current kubeconfig context
undeploy:
    helm uninstall scale-sentry || true

# Create a Kind cluster named {{kind_cluster}} (no-op if it already exists)
kind-create:
    #!/usr/bin/env bash
    set -euo pipefail
    if kind get clusters | grep -qx "{{kind_cluster}}"; then
      echo "✓ kind cluster {{kind_cluster}} already exists"
    else
      kind create cluster --name "{{kind_cluster}}"
    fi

# Delete the Kind cluster
kind-delete:
    kind delete cluster --name {{kind_cluster}} || true

# Load locally-built images into Kind
kind-load: docker-build
    kind load docker-image --name {{kind_cluster}} \
        scale-sentry:{{image_tag}} \
        scale-sentry-loadgen:{{image_tag}} \
        scale-sentry-observer:{{image_tag}}

# Full verdict E2E: build images, load into Kind (incl. hpa-example so
# scale-up does not trigger a 5x parallel registry.k8s.io pull storm
# that can starve the Kind control plane), install chart, run the suite.
test-e2e: kind-create kind-load deploy
    #!/usr/bin/env bash
    set -euo pipefail
    # The chart needs a moment to roll the controller pod out before tests
    # start asserting against it.
    kubectl wait --for=condition=Available --timeout=120s \
        -n default deployment/scale-sentry-controller
    # Pre-warm the target workload image inside the Kind node so HPA
    # scale-up does not have to pull it once per new replica.
    docker pull registry.k8s.io/hpa-example
    kind load docker-image --name {{kind_cluster}} registry.k8s.io/hpa-example
    go test -tags e2e -count=1 -timeout=15m ./test/e2e/...

# Spin up a dev cluster with the chart installed; no tests run, cluster stays up
dev-up: kind-create kind-load deploy
    @echo "✓ cluster up. apply a sample:  kubectl apply -f config/samples/targets/podinfo.yaml -f config/samples/scalevalidation-servicedefault.yaml"

# Tear down the dev cluster
dev-down: undeploy kind-delete

# Verify the chart's manager ClusterRole rules match config/rbac/role.yaml.
# The chart hand-templates RBAC so it can release-name-prefix the ClusterRole;
# this recipe is the drift gate that keeps it honest with the kubebuilder
# markers (CRDs + observer RBAC are symlinked, so they cannot drift at all).
chart-rbac-check:
    #!/usr/bin/env bash
    set -euo pipefail
    chart_rules=$(helm template scale-sentry charts/scale-sentry \
        --show-only templates/clusterrole.yaml 2>/dev/null | yq -o json '.rules')
    config_rules=$(yq -o json '.rules' config/rbac/role.yaml)
    if ! diff <(echo "$chart_rules") <(echo "$config_rules") >/dev/null; then
        echo "✗ chart manager RBAC has drifted from config/rbac/role.yaml"
        echo "  Run 'just manifests' then sync the rules block in"
        echo "  charts/scale-sentry/templates/clusterrole.yaml."
        diff <(echo "$chart_rules") <(echo "$config_rules") || true
        exit 1
    fi
    echo "✓ chart manager RBAC matches config/rbac/role.yaml"

# Install binaries via `go install` and reshim so mise exposes them immediately
install:
    go install ./cmd/...
    mise reshim 2>/dev/null || true
    @echo "installed → $(go env GOBIN)/{scale-sentry,loadgen}"

# Preview the next release without writing anything
release-preview bump="auto":
    @git cliff --bump {{bump}} --bumped-version | xargs -I{} echo "next: v{}"
    @echo "── changelog preview ──"
    @git cliff --bump {{bump}} --unreleased

# Bumps Chart.yaml (version + appVersion), generates CHANGELOG, tags, pushes, creates GH release. Args: auto|patch|minor|major|vX.Y.Z.
release bump="auto":
    #!/usr/bin/env bash
    set -euo pipefail
    if ! git diff-index --quiet HEAD --; then echo "✗ working tree dirty"; exit 1; fi
    case "{{bump}}" in
      v[0-9]*)                       new_ver="{{bump}}" ;;
      auto|patch|minor|major)        new_ver=$(git cliff --bump {{bump}} --bumped-version) ;;
      *) echo "usage: just release [auto|patch|minor|major|vX.Y.Z]"; exit 1 ;;
    esac
    case "$new_ver" in v*) ;; *) new_ver="v$new_ver" ;; esac
    if git rev-parse "$new_ver" >/dev/null 2>&1; then echo "✗ tag $new_ver already exists"; exit 1; fi
    chart_ver="${new_ver#v}"
    just check
    echo "▶ releasing $new_ver (chart + appVersion → $chart_ver)"
    yq -i ".version = \"$chart_ver\" | .appVersion = \"$chart_ver\"" charts/scale-sentry/Chart.yaml
    git cliff --tag "$new_ver" -o CHANGELOG.md
    git add CHANGELOG.md charts/scale-sentry/Chart.yaml
    git diff --cached --quiet || git commit -m "chore(release): $new_ver"
    git tag -a "$new_ver" -m "Release $new_ver"
    git push
    git push origin "refs/tags/$new_ver"
    notes=$(git cliff --tag "$new_ver" --latest --strip header)
    gh release create "$new_ver" --title "$new_ver" --notes "$notes" --verify-tag
