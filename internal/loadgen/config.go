package loadgen

import (
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// ConnectionMode selects how the prober reuses TCP/TLS connections.
type ConnectionMode string

const (
	// KeepAlive reuses pooled connections across requests (default fasthttp behaviour).
	KeepAlive ConnectionMode = "KeepAlive"
	// ShortLived sets Connection: close on every request so each call pays
	// TCP (and TLS, when applicable) handshake cost. Used to measure overhead.
	ShortLived ConnectionMode = "ShortLived"
)

// Protocol selects the HTTP wire protocol used to dispatch requests.
// HTTP1 uses fasthttp (the original loadgen client). HTTP2 uses net/http
// + golang.org/x/net/http2.Transport: a single TCP/TLS connection carries
// many concurrent streams, exercising the path real h2 services hit at
// scale (connection-pool dynamics, GOAWAY handling, stream-reset cost).
type Protocol string

const (
	ProtocolHTTP1 Protocol = "HTTP1"
	ProtocolHTTP2 Protocol = "HTTP2"
	// ProtocolGRPC dispatches each request as a
	// grpc.health.v1.Health/Check unary call via grpc-go. The cfg URL's
	// host:port is the gRPC server endpoint; the path is ignored.
	// Cleartext URLs (http://) dial plain TCP; https:// URLs dial with
	// the loadgen TLS config. The optional GRPCService scopes the
	// probe to a single per-service health entry.
	ProtocolGRPC Protocol = "GRPC"
)

// TargetMode mirrors api/v1alpha1.TargetConfig.Mode for result tagging.
// The loadgen package does not resolve modes, the controller resolves them
// and passes a concrete URL plus this label.
type TargetMode string

const (
	TargetServiceDefault TargetMode = "ServiceDefault"
	TargetAutoDiscover   TargetMode = "AutoDiscoverProbe"
	TargetCustomPath     TargetMode = "CustomPath"
)

// NetworkPath mirrors api/v1alpha1.TargetConfig.NetworkPath for result tagging.
type NetworkPath string

const (
	PathClusterIP NetworkPath = "ClusterIP"
	// PathIngress is the legacy classic-Ingress label; PathGateway is
	// preferred for new fixtures and is fed by Envoy Gateway and the
	// upstream Gateway API.
	PathIngress NetworkPath = "Ingress"
	PathGateway NetworkPath = "Gateway"
)

// Config is the full input to [Generator].
type Config struct {
	// URL is the fully-resolved absolute URL to probe. Required.
	URL string

	// Method is the HTTP method. Defaults to GET.
	Method string

	// Headers are added to every request.
	Headers map[string]string

	// TargetRPS is the steady-state requests-per-second target across all workers.
	// Must be > 0.
	TargetRPS int

	// Duration is the total wall-clock run length. Must be > 0.
	Duration time.Duration

	// Concurrency is the number of worker goroutines. If 0, defaults to
	// min(TargetRPS, 256).
	Concurrency int

	// ConnectionMode selects keep-alive vs short-lived connections.
	ConnectionMode ConnectionMode

	// Timeout is the per-request timeout. Defaults to 5s.
	Timeout time.Duration

	// TargetMode and NetworkPath are informational labels copied into [Result.Labels].
	TargetMode  TargetMode
	NetworkPath NetworkPath

	// TLSInsecureSkipVerify disables TLS certificate validation. Intended
	// for ingresses fronted by self-signed certs in dev / CI clusters; it
	// must never be set against production endpoints because the failure
	// signal scale-sentry watches for (TLS errors) is silently masked.
	TLSInsecureSkipVerify bool

	// TLSCABundlePath is the path to a PEM-encoded CA bundle the loadgen
	// adds to the client's RootCAs. Empty falls back to the system trust
	// store. Used for private CAs that issue ingress certs, so the
	// loadgen can validate them without InsecureSkipVerify.
	TLSCABundlePath string

	// Protocol selects the wire protocol. Defaults to ProtocolHTTP1
	// (fasthttp). ProtocolHTTP2 swaps in a net/http + http2.Transport
	// client. ProtocolGRPC swaps in a grpc-go client that drives the
	// standard health probe. All clients honor the same Timeout,
	// Headers, ConnectionMode, and TLS* fields where the protocol
	// supports them (HTTP-only knobs are silently no-ops on gRPC).
	Protocol Protocol

	// GRPCService is the upstream service name passed to the gRPC
	// Health/Check probe (HealthCheckRequest.service). Empty probes
	// overall server health. Consulted only when Protocol=GRPC.
	GRPCService string

	// Phases optionally replaces the single-shot Duration/TargetRPS pair
	// with an ordered list of arrival-rate segments (warmup, measure,
	// spike, etc.). When set, the generator ignores Duration and
	// TargetRPS: each Phase carries its own pattern + rate. When unset,
	// the generator synthesizes a single Constant phase from TargetRPS +
	// Duration so existing callers keep working unchanged.
	Phases []Phase
}

// Validate returns an error if cfg is missing required fields or holds
// values the generator cannot operate on. It does not mutate cfg.
func (cfg Config) Validate() error {
	if cfg.URL == "" {
		return errors.New("url is required")
	}
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return fmt.Errorf("parse url: %w", err)
	}
	if u.Scheme != "http" && u.Scheme != "https" {
		return fmt.Errorf("url scheme must be http or https, got %q", u.Scheme)
	}
	if u.Host == "" {
		return errors.New("url host is required")
	}
	if len(cfg.Phases) == 0 {
		// Legacy single-shot mode: TargetRPS + Duration are required.
		if cfg.TargetRPS <= 0 {
			return fmt.Errorf("targetRPS must be > 0, got %d", cfg.TargetRPS)
		}
		if cfg.Duration <= 0 {
			return fmt.Errorf("duration must be > 0, got %s", cfg.Duration)
		}
	} else {
		for i, p := range cfg.Phases {
			if err := p.Validate(); err != nil {
				return fmt.Errorf("phase[%d]: %w", i, err)
			}
		}
	}
	switch cfg.ConnectionMode {
	case KeepAlive, ShortLived:
	case "":
		return errors.New("connectionMode is required")
	default:
		return fmt.Errorf("unknown connectionMode %q", cfg.ConnectionMode)
	}
	switch cfg.Protocol {
	case "", ProtocolHTTP1, ProtocolHTTP2, ProtocolGRPC:
	default:
		return fmt.Errorf("unknown protocol %q", cfg.Protocol)
	}
	if cfg.Method != "" {
		if _, err := http.NewRequest(cfg.Method, cfg.URL, nil); err != nil {
			return fmt.Errorf("invalid method %q: %w", cfg.Method, err)
		}
	}
	return nil
}

// withDefaults returns a copy of cfg with zero-value defaults filled in.
// When Phases is empty, a single Constant phase is synthesized from
// TargetRPS + Duration so the rest of the generator can operate on a
// uniform phase list regardless of how the caller specified the load.
func (cfg Config) withDefaults() Config {
	if cfg.Method == "" {
		cfg.Method = http.MethodGet
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.Protocol == "" {
		cfg.Protocol = ProtocolHTTP1
	}
	if len(cfg.Phases) == 0 {
		cfg.Phases = []Phase{{
			Name:        MeasurePhaseName,
			Pattern:     PatternConstant,
			Duration:    cfg.Duration,
			StartRPS:    cfg.TargetRPS,
			RecordStats: true,
		}}
	}
	if cfg.Concurrency == 0 {
		peak := 0
		for _, p := range cfg.Phases {
			if r := p.peakRPS(); r > peak {
				peak = r
			}
		}
		cfg.Concurrency = peak
		if cfg.Concurrency > 256 {
			cfg.Concurrency = 256
		}
	}
	return cfg
}
