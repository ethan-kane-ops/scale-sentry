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

// TargetMode mirrors api/v1alpha1.TargetConfig.Mode for result tagging.
// The loadgen package does not resolve modes — the controller resolves them
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
	PathIngress   NetworkPath = "Ingress"
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
	if cfg.TargetRPS <= 0 {
		return fmt.Errorf("targetRPS must be > 0, got %d", cfg.TargetRPS)
	}
	if cfg.Duration <= 0 {
		return fmt.Errorf("duration must be > 0, got %s", cfg.Duration)
	}
	switch cfg.ConnectionMode {
	case KeepAlive, ShortLived:
	case "":
		return errors.New("connectionMode is required")
	default:
		return fmt.Errorf("unknown connectionMode %q", cfg.ConnectionMode)
	}
	if cfg.Method != "" {
		if _, err := http.NewRequest(cfg.Method, cfg.URL, nil); err != nil {
			return fmt.Errorf("invalid method %q: %w", cfg.Method, err)
		}
	}
	return nil
}

// withDefaults returns a copy of cfg with zero-value defaults filled in.
func (cfg Config) withDefaults() Config {
	if cfg.Method == "" {
		cfg.Method = http.MethodGet
	}
	if cfg.Timeout == 0 {
		cfg.Timeout = 5 * time.Second
	}
	if cfg.Concurrency == 0 {
		cfg.Concurrency = cfg.TargetRPS
		if cfg.Concurrency > 256 {
			cfg.Concurrency = 256
		}
	}
	return cfg
}
