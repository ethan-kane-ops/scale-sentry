# Target Cookbook

Recipes for the most common `spec.target` shapes. Pick the row that matches your workload's wire protocol and edge style, then copy the YAML and adjust names / ports.

## At a glance

| Protocol | ClusterIP (Service)         | Gateway (Envoy)             | Ingress (classic, legacy)   |
|----------|-----------------------------|-----------------------------|-----------------------------|
| HTTP/1.1 | [#http1-clusterip][1]       | [#http1-gateway][2]         | [#http1-ingress][3]         |
| HTTP/2   | [#http2-clusterip][4]       | [#http2-gateway][5]         | n/a                         |
| gRPC     | [#grpc-clusterip][6]        | [#grpc-gateway][7]          | n/a                         |

[1]: #http1-clusterip
[2]: #http1-gateway
[3]: #http1-ingress
[4]: #http2-clusterip
[5]: #http2-gateway
[6]: #grpc-clusterip
[7]: #grpc-gateway

Working copies of every Gateway-path recipe ship in `config/e2e/` along with the target Deployments, Envoy Gateway resources, and a README that walks through install + run + teardown.

## HTTP/1.1 {#http1}

### ClusterIP {#http1-clusterip}

The default. `spec.target.host` is left unset; the controller resolves to `<targetRef.name>.<namespace>.svc.cluster.local`.

```yaml
spec:
  target:
    mode: ServiceDefault
    port: 80
    networkPath: ClusterIP
    protocol: HTTP1
```

### Gateway (Envoy) {#http1-gateway}

Point `host` at the Envoy Gateway listener Service. The HTTPRoute attached to that listener handles the upstream lookup.

```yaml
spec:
  target:
    mode: ServiceDefault
    port: 80
    networkPath: Gateway
    protocol: HTTP1
    host: envoy-<gateway-ns>-<gateway-name>-<hash>.envoy-gateway-system.svc.cluster.local
```

The Envoy Gateway controller names the listener Service `envoy-<gw-ns>-<gw-name>-<hash>` by default; look it up with:

```bash
kubectl -n envoy-gateway-system get svc -l gateway.envoyproxy.io/owning-gateway-name=<gw-name>
```

### Classic Ingress {#http1-ingress}

Legacy path. Prefer Gateway for new workloads; classic `kubernetes/ingress-nginx` is sunset.

```yaml
spec:
  target:
    mode: ServiceDefault
    port: 80
    networkPath: Ingress
    protocol: HTTP1
    host: my-app.example.com   # the Ingress rule's host
```

## HTTP/2 {#http2}

### ClusterIP {#http2-clusterip}

Cleartext HTTP/2 (h2c). The loadgen dials `http://` and switches into h2c prior-knowledge framing because `protocol: HTTP2`.

```yaml
spec:
  target:
    mode: ServiceDefault
    port: 80
    networkPath: ClusterIP
    protocol: HTTP2
```

The backend must accept h2c. Servers that speak it include nginx (`http2 on` on a cleartext listener), Caddy (`servers { protocols h1 h2c }`), and Envoy upstreams with h2 framing. Go `net/http` servers (including traefik/whoami) only speak h2 over TLS; the repo's h2c fixture uses Caddy for exactly that reason (`config/e2e/targets/h2c-echo.yaml`).

### Gateway (Envoy) {#http2-gateway}

Same as the HTTP/1 Gateway recipe but with `protocol: HTTP2`. The upstream Service should mark `appProtocol: kubernetes.io/h2c` so Envoy keeps h2 framing end-to-end rather than downgrading to HTTP/1 upstream.

```yaml
spec:
  target:
    mode: ServiceDefault
    port: 8080
    networkPath: Gateway
    protocol: HTTP2
    host: envoy-<gateway-ns>-<gateway-name>-<hash>.envoy-gateway-system.svc.cluster.local
```

## gRPC {#grpc}

The loadgen drives the standard `grpc.health.v1.Health/Check` probe. The target must register the Health server. URL path is ignored by gRPC; the loadgen extracts host:port and discards the rest.

### ClusterIP {#grpc-clusterip}

```yaml
spec:
  target:
    mode: ServiceDefault
    port: 50051
    networkPath: ClusterIP
    protocol: GRPC
    grpc:
      service: orders.v1.Orders   # optional, scopes the probe to a per-service entry
```

### Gateway (Envoy) {#grpc-gateway}

Attach a `GRPCRoute` to the Gateway listener, then point the validation at the listener Service:

```yaml
spec:
  target:
    mode: ServiceDefault
    port: 50051
    networkPath: Gateway
    protocol: GRPC
    host: envoy-<gateway-ns>-<gateway-name>-<hash>.envoy-gateway-system.svc.cluster.local
    grpc:
      service: orders.v1.Orders
```

Envoy's `GRPCRoute` keeps h2 framing end-to-end. The upstream Service should mark `appProtocol: grpc` to make that explicit.

## TLS

All recipes above default to cleartext. Add TLS verification by setting `https://` indirectly through the resolved scheme (currently only via `mode: AutoDiscoverProbe` reading the backend's readiness `httpGet.scheme`), or by trusting a private CA via a ConfigMap reference:

```yaml
spec:
  target:
    # ...
    tls:
      caBundle:
        configMapRef:
          name: my-cluster-ca
          key: ca.crt
```

`tls.insecureSkipVerify: true` exists for dev clusters but is loud about masking TLS errors; do not enable it on production runs.

## CRDs the cookbook does not cover

- Custom path matching for upstream-rewritten routes (`mode: CustomPath` + a non-`/` `customPath`).
- `AutoDiscoverProbe` mode that reads the target Deployment's readiness probe to pick port + path + scheme automatically.

See the [Configuration reference](configuration.md) for both.
