package loadgen

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"github.com/valyala/fasthttp"
)

// protocolClient is the abstraction over the loadgen's request dispatcher.
// Two implementations exist today: an HTTP/1.1 client backed by fasthttp
// (h1Client) and an HTTP/2 client backed by net/http + http2.Transport
// (h2Client). The interface is intentionally small, just enough for the
// per-phase worker loop and the post-run stats roll-up, so adding a third
// protocol (gRPC, planned) only requires implementing it.
type protocolClient interface {
	// Do issues one request. status is the HTTP response code (or a
	// protocol-mapped equivalent for non-HTTP wire formats); 0 on
	// transport error. ctx bounds dispatch + read; per-request timeout
	// is also baked into the client.
	Do(ctx context.Context) (status int, err error)
	// Stats returns the protocol-specific counters accumulated across
	// every Do call. Safe to read after the run finishes; values during
	// a live run are best-effort.
	Stats() ClientStats
}

// ClientStats holds counters the loadgen exposes in Result.Labels for
// post-run analysis. h1Client always reports zeroes; h2Client tracks
// real values.
type ClientStats struct {
	// GoAwayCount is the number of GOAWAY frames the server sent.
	// HTTP/1 is always 0; HTTP/2 elevated values indicate the server is
	// shedding connections (rolling restart, overload, conn limits).
	GoAwayCount int64
	// ConnsOpened is the count of new TCP/TLS connections opened during
	// the run, sampled via httptrace.ClientTrace.GotConn (Reused=false).
	ConnsOpened int64
	// StreamsIssued is the count of streams started; ratio with
	// ConnsOpened approximates streams-per-connection for HTTP/2.
	StreamsIssued int64
}

// newProtocolClient returns the protocolClient matching cfg.Protocol.
// HTTP1 keeps fasthttp; HTTP2 uses net/http + http2.Transport. The
// returned client honors cfg's Timeout, Headers, Method, ConnectionMode,
// and TLS* fields.
func newProtocolClient(cfg Config) (protocolClient, error) {
	switch cfg.Protocol {
	case "", ProtocolHTTP1:
		return newH1Client(cfg)
	case ProtocolHTTP2:
		return newH2Client(cfg)
	default:
		return nil, fmt.Errorf("unknown protocol %q", cfg.Protocol)
	}
}

// h1Client wraps a fasthttp.Client. It is the original loadgen client
// promoted to satisfy the protocolClient interface so the Generator
// dispatch path is protocol-agnostic.
type h1Client struct {
	cfg    Config
	client *fasthttp.Client
}

// newH1Client builds the fasthttp-backed client. ShortLived sets
// MaxConnDuration to 1ns so fasthttp dials fresh on every call (combined
// with the worker's Connection: close header it forces a real handshake
// per request, the way the user asked for in the CR).
func newH1Client(cfg Config) (*h1Client, error) {
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
		c.MaxConnDuration = time.Nanosecond
	}
	return &h1Client{cfg: cfg, client: c}, nil
}

// Do issues one HTTP/1.1 request via fasthttp, applying the worker's
// Connection: close header on ShortLived runs.
func (h *h1Client) Do(_ context.Context) (int, error) {
	req := fasthttp.AcquireRequest()
	resp := fasthttp.AcquireResponse()
	defer fasthttp.ReleaseRequest(req)
	defer fasthttp.ReleaseResponse(resp)

	req.SetRequestURI(h.cfg.URL)
	req.Header.SetMethod(h.cfg.Method)
	for k, v := range h.cfg.Headers {
		req.Header.Set(k, v)
	}
	if h.cfg.ConnectionMode == ShortLived {
		req.Header.SetConnectionClose()
	}
	if err := h.client.DoTimeout(req, resp, h.cfg.Timeout); err != nil {
		return 0, err
	}
	return resp.StatusCode(), nil
}

// Stats reports zeroes; fasthttp does not surface per-connection stream
// counts, and HTTP/1 has no GOAWAY equivalent.
func (h *h1Client) Stats() ClientStats { return ClientStats{} }

// classify maps a transport-level dispatch error to an ErrorCategory.
// Shared across h1 and h2 paths so error taxonomy stays consistent.
func classify(err error) ErrorCategory {
	if err == nil {
		return ErrOther
	}
	if errors.Is(err, fasthttp.ErrTimeout) || errors.Is(err, fasthttp.ErrDialTimeout) {
		return ErrTimeout
	}
	if errors.Is(err, context.DeadlineExceeded) {
		return ErrTimeout
	}
	msg := err.Error()
	switch {
	case strings.Contains(msg, "GOAWAY"):
		// http2 surfaces GOAWAY via the http2.GoAwayError wrapper; map
		// to ErrServer per the ENG-56 design (treat as upstream 5xx).
		return ErrServer
	case strings.Contains(msg, "stream error"), strings.Contains(msg, "RST_STREAM"):
		return ErrStreamReset
	case strings.Contains(msg, "connection reset"):
		return ErrConnReset
	case strings.Contains(msg, "tls:"), strings.Contains(msg, "x509:"):
		return ErrTLS
	case strings.Contains(msg, "dial"), strings.Contains(msg, "no such host"):
		return ErrDial
	}
	return ErrOther
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

// atomicCounter wraps int64 in an atomic-loadable form. Used by h2Client
// to expose its httptrace counters via Stats() without a mutex hot-path.
type atomicCounter struct{ v int64 }

func (a *atomicCounter) Add(delta int64) { atomic.AddInt64(&a.v, delta) }
func (a *atomicCounter) Load() int64     { return atomic.LoadInt64(&a.v) }
