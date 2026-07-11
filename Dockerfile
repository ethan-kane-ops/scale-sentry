# One Dockerfile, three images. Pass --build-arg CMD=<name> to select the
# binary (scale-sentry | loadgen | observer); the justfile docker-build
# recipe builds all three.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:0178a641fbb4858c5f1b48e34bdaabe0350a330a1b1149aabd498d0699ff5fb2 AS build
ARG TARGETOS
ARG TARGETARCH
ARG CMD=scale-sentry
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/entrypoint ./cmd/${CMD}

FROM gcr.io/distroless/static-debian12:nonroot@sha256:b7bb25d9f7c31d2bdd1982feb4dafcaf137703c7075dbe2febb41c24212b946f
ARG CMD=scale-sentry
LABEL org.opencontainers.image.source="https://github.com/ethan-kane-ops/scale-sentry"
LABEL org.opencontainers.image.description="scale-sentry: ${CMD}"
COPY --from=build /out/entrypoint /entrypoint
USER 65532:65532
ENTRYPOINT ["/entrypoint"]
