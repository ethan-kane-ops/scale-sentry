# One Dockerfile, three images. Pass --build-arg CMD=<name> to select the
# binary (scale-sentry | loadgen | observer); the justfile docker-build
# recipe builds all three.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine AS build
ARG TARGETOS
ARG TARGETARCH
ARG CMD=scale-sentry
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/entrypoint ./cmd/${CMD}

FROM gcr.io/distroless/static-debian12:nonroot
ARG CMD=scale-sentry
LABEL org.opencontainers.image.source="https://github.com/ethan-kane-ops/scale-sentry"
LABEL org.opencontainers.image.description="scale-sentry: ${CMD}"
COPY --from=build /out/entrypoint /entrypoint
USER 65532:65532
ENTRYPOINT ["/entrypoint"]
