package loadgen

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptrace"
	"strings"
)

// h2Client speaks HTTP/2. For https:// URLs ALPN negotiates h2; for
// http:// URLs the client uses h2c prior-knowledge (no upgrade dance,
// dial straight into a binary-framed connection). It uses net/http's
// own HTTP/2 transport, restricted to HTTP/2 via Transport.Protocols, so
// GOAWAY surfacing and per-stream cancellation come for free.
//
// Connection / stream stats are tracked via httptrace.ClientTrace:
// every successful GotConn fires, so we can distinguish a new TCP/TLS
// connection (Reused=false → ConnsOpened++) from a multiplexed stream
// on an existing one (Reused=true → only StreamsIssued++). The ratio
// approximates streams-per-connection. GOAWAY count comes from the
// request error: net/http's HTTP/2 GOAWAY error type is unexported, so
// it is matched by its message, as classify() does.
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
	// HTTP/2 only, HTTP/1.1 off: an https:// URL negotiates h2 via ALPN
	// and a server lacking h2 hard-fails rather than silently
	// downgrading to HTTP/1.1 and confusing the verdict. An http:// URL
	// uses h2c prior knowledge (unencrypted HTTP/2, no upgrade).
	protocols := new(http.Protocols)
	protocols.SetHTTP2(true)
	protocols.SetUnencryptedHTTP2(true)

	transport := &http.Transport{
		TLSClientConfig: tlsCfg,
		Protocols:       protocols,
		HTTP2: &http.HTTP2Config{
			SendPingTimeout: cfg.Timeout,
			PingTimeout:     cfg.Timeout,
		},
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
		// Count GOAWAY errors so the post-run stats reflect server-side
		// connection shedding.
		if strings.Contains(err.Error(), "server sent GOAWAY") {
			h.stats.goAway.Add(1)
		}
		return 0, err
	}
	defer func() { _ = resp.Body.Close() }()
	// Drain the body so the underlying stream is fully released back to
	// the connection pool; otherwise the transport keeps the stream
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

// Close drops idle HTTP/2 connections.
func (h *h2Client) Close() { h.client.CloseIdleConnections() }

// Compile-time guard: every protocolClient must satisfy the contract,
// and the package's New() depends on h1Client/h2Client both meeting it.
var (
	_ protocolClient = (*h1Client)(nil)
	_ protocolClient = (*h2Client)(nil)
)
