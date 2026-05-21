# Version pins — keep in sync with go.mod
controller_gen_version := "v0.16.5"

default:
    @just --list

# Build all binaries into ./bin/ (isolated — does not affect the installed binary)
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

# Run the locally built binary — safe during development, never touches the installed version
run *args: build
    ./bin/scale-sentry {{args}}

# Run all tests
test:
    go test ./...

# Run tests with race detector
test-race:
    go test -race ./...

# Run linters
lint:
    go vet ./...
    golangci-lint run

# Tidy go modules
tidy:
    go mod tidy

# Tidy + lint + test
check: tidy lint test

# Remove build artifacts
clean:
    rm -rf bin/

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

# Cut a release: bump (auto/patch/minor/major) or explicit vX.Y.Z. Generates CHANGELOG.md, tags, pushes, gh release.
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
    just check
    echo "▶ releasing $new_ver"
    git cliff --tag "$new_ver" -o CHANGELOG.md
    git add CHANGELOG.md
    git diff --cached --quiet || git commit -m "chore(release): $new_ver"
    git tag -a "$new_ver" -m "Release $new_ver"
    git push
    git push origin "refs/tags/$new_ver"
    notes=$(git cliff --tag "$new_ver" --latest --strip header)
    gh release create "$new_ver" --title "$new_ver" --notes "$notes" --verify-tag
