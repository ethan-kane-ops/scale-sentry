# One Dockerfile, three images. Pass --build-arg CMD=<name> to select the
# binary (scale-sentry | loadgen | observer); the justfile docker-build
# recipe builds all three.
FROM --platform=$BUILDPLATFORM golang:1.26-alpine@sha256:3889b425f035be855a72fb4755265311293b6d414521f0a519d819df32222d83 AS build
ARG TARGETOS
ARG TARGETARCH
ARG CMD=scale-sentry
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/entrypoint ./cmd/${CMD}

FROM gcr.io/distroless/static-debian12:nonroot@sha256:1b7b9f0f0e0a1d2155f531db587cc48ec26aaf97ab64364225f5bf18a054e66a
ARG CMD=scale-sentry
LABEL org.opencontainers.image.source="https://github.com/ethan-kane-ops/scale-sentry"
LABEL org.opencontainers.image.description="scale-sentry: ${CMD}"
COPY --from=build /out/entrypoint /entrypoint
USER 65532:65532
ENTRYPOINT ["/entrypoint"]
