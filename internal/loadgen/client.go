package loadgen

import (
	"crypto/tls"
	"crypto/x509"
	"fmt"
	"os"
	"time"

	"github.com/valyala/fasthttp"
)

// newClient returns a fasthttp.Client configured for the given connection mode.
//
// ShortLived disables connection reuse via MaxConnDuration = 1ns, forcing
// fasthttp to close and re-dial on every request. KeepAlive uses the
// default pool settings.
func newClient(cfg Config) (*fasthttp.Client, error) {
	tlsCfg, err := buildTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	c := &fasthttp.Client{
		ReadTimeout:                   cfg.Timeout,
		WriteTimeout:                  cfg.Timeout,
		MaxIdemponentCallAttempts:     1,
		DisableHeaderNamesNormalizing: false,
		TLSConfig:                     tlsCfg,
	}
	if cfg.ConnectionMode == ShortLived {
		// Force per-request reconnect. 1ns guarantees the connection has
		// "expired" by the time we'd try to reuse it on the next call,
		// so fasthttp dials fresh. Combined with Connection: close header
		// set in the prober, this exercises full TCP/TLS handshake cost.
		c.MaxConnDuration = time.Nanosecond
	}
	return c, nil
}

// buildTLSConfig assembles the TLS config from cfg's TLS knobs. Defaults
// to TLS 1.2+ with the system trust store; an explicit CA bundle replaces
// the system pool, and InsecureSkipVerify disables verification entirely.
// The two are mutually compatible, Skip wins, but the loadgen warns
// loudly via the result labels when Skip is on so misuse is visible.
func buildTLSConfig(cfg Config) (*tls.Config, error) {
	t := &tls.Config{MinVersion: tls.VersionTLS12}
	if cfg.TLSInsecureSkipVerify {
		t.InsecureSkipVerify = true
	}
	if cfg.TLSCABundlePath != "" {
		pem, err := os.ReadFile(cfg.TLSCABundlePath)
		if err != nil {
			return nil, fmt.Errorf("read TLS CA bundle %s: %w", cfg.TLSCABundlePath, err)
		}
		pool := x509.NewCertPool()
		if !pool.AppendCertsFromPEM(pem) {
			return nil, fmt.Errorf("TLS CA bundle %s contained no valid PEM certificates", cfg.TLSCABundlePath)
		}
		t.RootCAs = pool
	}
	return t, nil
}
