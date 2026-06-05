# Protocols

Scale Sentry's loadgen speaks three wire protocols. `spec.target.protocol` picks one per run; the underlying dispatcher swaps out without affecting the phase + arrival-pattern logic above it.

| Value   | Stack                                       | When to use                                                                                            |
|---------|---------------------------------------------|--------------------------------------------------------------------------------------------------------|
| `HTTP1` | fasthttp                                    | Default. Best raw throughput for plain HTTP/1.1 backends; lowest CPU overhead in the loadgen.          |
| `HTTP2` | `net/http` + `golang.org/x/net/http2`       | HTTP/2 backends (gRPC services that also serve JSON, Envoy-fronted apps, multiplexed-stream workloads).|
| `GRPC`  | `google.golang.org/grpc` Health/Check probe | Pure gRPC backends. Drives the canonical Health probe; no per-target proto stubs required.             |

## HTTP/1.1

Default. fasthttp is faster and lighter than `net/http`; trade-off is no streaming-body support and no h2 negotiation. `ShortLived` connection mode pairs with `MaxConnDuration=1ns` so the client dials fresh on every request, exercising the full TCP/TLS handshake cost.

```yaml
spec:
  target:
    protocol: HTTP1   # or omit
```

## HTTP/2

ALPN-negotiated `h2` over TLS (`https://` URLs), prior-knowledge `h2c` over cleartext (`http://` URLs). The same `Protocol: HTTP2` setting handles both: `AllowHTTP=true` + a custom `DialTLSContext` closure routes cleartext to raw TCP framing and TLS to `tls.Client`.

```yaml
spec:
  target:
    protocol: HTTP2
```

`ShortLived` HTTP/2 sets per-request `Connection: close` + `req.Close=true` so the server tears the connection down once the body is fully read. The Go `http2.Transport` does not expose a `MaxConnsPerHost` knob; the close header is the lever.

### HTTP/2 result labels

| Label           | Meaning                                                                              |
|-----------------|--------------------------------------------------------------------------------------|
| `connsOpened`   | New TCP/TLS connections (httptrace `GotConn` with `Reused=false`)                    |
| `streamsIssued` | Total streams started (every `GotConn` regardless of `Reused`)                       |
| `goAwayCount`   | GOAWAY frames the server sent (detected via `errors.As(*http2.GoAwayError)`)         |

Elevated `goAwayCount` is the canonical signal of a target shedding connections under load (rolling restart, conn limits, overload). The `streamsIssued / connsOpened` ratio approximates streams per connection: `KeepAlive` typically lands at 50+, `ShortLived` near 1.

## gRPC

`Protocol: GRPC` swaps in a grpc-go client that drives the standard `grpc.health.v1.Health/Check` probe at the configured rate. The probe is intentionally narrow: the goal is to exercise h2 framing + gRPC trailers + backend goroutines, not to invoke arbitrary RPC methods. Reflection-based method discovery and unary calls against arbitrary services are deferred to a future ticket.

```yaml
spec:
  target:
    protocol: GRPC
    grpc:
      service: orders.v1.Orders   # optional, empty probes overall health
```

The target Service must register the standard Health server. One-liner for grpc-go:

```go
healthpb.RegisterHealthServer(srv, health.NewServer())
```

Equivalents exist for grpc-java, grpc-python, grpc-rust, and so on. Kubelet's `grpc` probe uses the same Health/Check.

### gRPC result labels

| Label           | Meaning                                                                          |
|-----------------|----------------------------------------------------------------------------------|
| `connsOpened`   | New gRPC subconns established (`stats.Handler` `ConnBegin` events)               |
| `streamsIssued` | Total RPCs dispatched (`stats.Handler` `Begin` events)                           |
| `grpcService`   | The `HealthCheckRequest.service` value, only present when non-empty              |

### gRPC status mapping

gRPC status codes are mapped to nearest HTTP equivalents so the histogram, status counters, and SLA verdict stay protocol-agnostic. A `NOT_SERVING` Health response (technically an OK status with an unhealthy payload) is surfaced as **503**.

| gRPC Code              | HTTP |
|------------------------|------|
| `OK`                   | 200  |
| `Canceled`             | 499  |
| `InvalidArgument`      | 400  |
| `Unauthenticated`      | 401  |
| `PermissionDenied`     | 403  |
| `NotFound`             | 404  |
| `AlreadyExists`        | 409  |
| `FailedPrecondition`   | 412  |
| `ResourceExhausted`    | 429  |
| `Internal` / `Unknown` | 500  |
| `Unimplemented`        | 501  |
| `Unavailable`          | 503  |
| `DeadlineExceeded`     | 504  |
