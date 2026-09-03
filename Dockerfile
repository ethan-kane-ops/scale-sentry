# One Dockerfile, three images. Pass --build-arg CMD=<name> to select the
# binary (scale-sentry | loadgen | observer); the justfile docker-build
# recipe builds all three.
FROM --platform=$BUILDPLATFORM golang:1.27-alpine@sha256:cf6fca6641884b8433441b2b0652976f975e1d0fdd26d177eaaf8596087f3125 AS build
ARG TARGETOS
ARG TARGETARCH
ARG CMD=scale-sentry
WORKDIR /app
COPY go.mod go.sum ./
RUN go mod download
COPY . .
RUN CGO_ENABLED=0 GOOS=$TARGETOS GOARCH=$TARGETARCH \
    go build -trimpath -ldflags="-s -w" -o /out/entrypoint ./cmd/${CMD}

FROM gcr.io/distroless/static-debian12:nonroot@sha256:afa5c872c891853ca7fcf1f12c3edb23f7eeef36189728842dd51042ff57f7ab
ARG CMD=scale-sentry
LABEL org.opencontainers.image.source="https://github.com/ethan-kane-ops/scale-sentry"
LABEL org.opencontainers.image.description="scale-sentry: ${CMD}"
COPY --from=build /out/entrypoint /entrypoint
USER 65532:65532
ENTRYPOINT ["/entrypoint"]
