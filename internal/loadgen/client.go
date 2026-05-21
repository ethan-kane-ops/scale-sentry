package loadgen

import (
	"crypto/tls"
	"time"

	"github.com/valyala/fasthttp"
)

// newClient returns a fasthttp.Client configured for the given connection mode.
//
// ShortLived disables connection reuse via MaxConnDuration = 1ns, forcing
// fasthttp to close and re-dial on every request. KeepAlive uses the
// default pool settings.
func newClient(mode ConnectionMode, timeout time.Duration) *fasthttp.Client {
	c := &fasthttp.Client{
		ReadTimeout:                   timeout,
		WriteTimeout:                  timeout,
		MaxIdemponentCallAttempts:     1,
		DisableHeaderNamesNormalizing: false,
		TLSConfig:                     &tls.Config{MinVersion: tls.VersionTLS12},
	}
	if mode == ShortLived {
		// Force per-request reconnect. 1ns guarantees the connection has
		// "expired" by the time we'd try to reuse it on the next call,
		// so fasthttp dials fresh. Combined with Connection: close header
		// set in the prober, this exercises full TCP/TLS handshake cost.
		c.MaxConnDuration = time.Nanosecond
	}
	return c
}
