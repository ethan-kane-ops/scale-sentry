package loadgen

import (
	"context"
	"crypto/tls"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/http/httptrace"
	"strings"

	"golang.org/x/net/http2"
)

// h2Client speaks HTTP/2. For https:// URLs ALPN negotiates h2; for
// http:// URLs the client uses h2c prior-knowledge (no upgrade dance,
// dial straight into a binary-framed connection). It wraps net/http +
// http2.Transport so we get GOAWAY surfacing and per-stream
// cancellation for free.
//
// Connection / stream stats are tracked via httptrace.ClientTrace:
// every successful GotConn fires, so we can distinguish a new TCP/TLS
// connection (Reused=false → ConnsOpened++) from a multiplexed stream
// on an existing one (Reused=true → only StreamsIssued++). The ratio
// approximates streams-per-connection. GOAWAY count is read from the
// http2.GoAwayError wrapper since the Transport does not expose a
// public callback.
type h2Client struct {
	cfg    Config
	client *http.Client
	stats  h2Stats
}

// h2Stats holds the atomic counters fed by httptrace + error
// classification. Read via Stats(); written by request goroutines.
type h2Stats struct {
	conns   atomicCounter
	streams atomicCounter
	goAway  atomicCounter
}

// newH2Client builds the HTTP/2 dispatcher. ShortLived sets the
// per-request Connection: close header and req.Close=true so the
// server tears the connection down once the response body is fully
// read; combined with the worker's per-request dispatch this exercises
// the full TCP/TLS handshake cost (the point of ShortLived).
func newH2Client(cfg Config) (*h2Client, error) {
	tlsCfg, err := buildTLSConfig(cfg)
	if err != nil {
		return nil, err
	}
	// Advertise h2 in ALPN. An https:// URL will then negotiate HTTP/2
	// with the server; a server lacking h2 hard-fails here rather than
	// silently downgrading to HTTP/1.1 and confusing the verdict.
	tlsCfg.NextProtos = []string{"h2"}

	// Capture the URL scheme by value so the DialTLSContext closure
	// does not have to inspect the request later. AllowHTTP=true makes
	// http2.Transport route http:// dials through DialTLSContext as
	// well, so the closure has to handle both schemes.
	isHTTPS := strings.HasPrefix(cfg.URL, "https://")

	transport := &http2.Transport{
		TLSClientConfig: tlsCfg,
		AllowHTTP:       true,
		DialTLSContext: func(ctx context.Context, network, addr string, dialCfg *tls.Config) (net.Conn, error) {
			var d net.Dialer
			conn, err := d.DialContext(ctx, network, addr)
			if err != nil {
				return nil, err
			}
			if !isHTTPS {
				// h2c prior-knowledge: hand back the raw TCP conn,
				// http2.Transport will run HTTP/2 framing over it.
				return conn, nil
			}
			tlsConn := tls.Client(conn, dialCfg)
			if err := tlsConn.HandshakeContext(ctx); err != nil {
				_ = conn.Close()
				return nil, err
			}
			return tlsConn, nil
		},
		ReadIdleTimeout: cfg.Timeout,
		PingTimeout:     cfg.Timeout,
	}
	c := &h2Client{
		cfg: cfg,
		client: &http.Client{
			Transport: transport,
			Timeout:   cfg.Timeout,
		},
	}
	return c, nil
}

// Do issues one h2 request. The httptrace hook tags every successful
// GotConn against the conns/streams atomics; errors are classified by
// classify(). On ShortLived runs the Connection: close header is set so
// the server tears the conn down once the response body is fully read.
func (h *h2Client) Do(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, h.cfg.Timeout)
	defer cancel()

	trace := &httptrace.ClientTrace{
		GotConn: func(info httptrace.GotConnInfo) {
			h.stats.streams.Add(1)
			if !info.Reused {
				h.stats.conns.Add(1)
			}
		},
	}
	ctx = httptrace.WithClientTrace(ctx, trace)

	req, err := http.NewRequestWithContext(ctx, h.cfg.Method, h.cfg.URL, nil)
	if err != nil {
		return 0, fmt.Errorf("build h2 request: %w", err)
	}
	for k, v := range h.cfg.Headers {
		req.Header.Set(k, v)
	}
	if h.cfg.ConnectionMode == ShortLived {
		req.Header.Set("Connection", "close")
		req.Close = true
	}
	resp, err := h.client.Do(req)
	if err != nil {
		// Map GOAWAY-wrapped errors into the goAway counter so the
		// post-run stats reflect server-side connection shedding.
		var goAwayErr *http2.GoAwayError
		if errors.As(err, &goAwayErr) {
			h.stats.goAway.Add(1)
		}
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain the body so the underlying stream is fully released back to
	// the connection pool; otherwise http2.Transport keeps the stream
	// open and subsequent requests on the same conn stall.
	_, _ = io.Copy(io.Discard, resp.Body)
	return resp.StatusCode, nil
}

// Stats returns a snapshot of the httptrace + GOAWAY counters.
func (h *h2Client) Stats() ClientStats {
	return ClientStats{
		GoAwayCount:   h.stats.goAway.Load(),
		ConnsOpened:   h.stats.conns.Load(),
		StreamsIssued: h.stats.streams.Load(),
	}
}

// Compile-time guard: every protocolClient must satisfy the contract,
// and the package's New() depends on h1Client/h2Client both meeting it.
var (
	_ protocolClient = (*h1Client)(nil)
	_ protocolClient = (*h2Client)(nil)
)
