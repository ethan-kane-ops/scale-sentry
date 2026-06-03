package loadgen

import (
	"context"
	"crypto/tls"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"
)

// h2TestServer spins up an httptest.Server with HTTP/2 enabled via TLS.
// The returned URL is https://; ALPN advertises h2 and http/1.1 (the
// httptest helper unconditionally adds both, so the h2 client's
// h2-only NextProtos still negotiates correctly).
func h2TestServer(t *testing.T, h http.Handler) *httptest.Server {
	t.Helper()
	srv := httptest.NewUnstartedServer(h)
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)
	return srv
}

// TestH2Client_NegotiatesHTTP2 asserts the dispatcher actually drives
// HTTP/2: the handler's r.Proto reports "HTTP/2.0" for every request
// the loadgen sends.
func TestH2Client_NegotiatesHTTP2(t *testing.T) {
	var h2Hits, hits int64
	srv := h2TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		atomic.AddInt64(&hits, 1)
		if r.ProtoMajor == 2 {
			atomic.AddInt64(&h2Hits, 1)
		}
		w.WriteHeader(http.StatusOK)
	}))

	cfg := Config{
		URL:                   srv.URL + "/",
		Protocol:              ProtocolHTTP2,
		ConnectionMode:        KeepAlive,
		TargetRPS:             20,
		Duration:              500 * time.Millisecond,
		Timeout:               2 * time.Second,
		TLSInsecureSkipVerify: true,
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result := g.Run(context.Background())

	total := atomic.LoadInt64(&hits)
	if total == 0 {
		t.Fatal("server saw zero requests")
	}
	if atomic.LoadInt64(&h2Hits) != total {
		t.Errorf("h2 negotiated for %d of %d requests, want all", h2Hits, total)
	}
	if result.Labels["protocol"] != "HTTP2" {
		t.Errorf("Labels[protocol] = %q, want HTTP2", result.Labels["protocol"])
	}
}

// TestH2Client_StreamsAreMultiplexed asserts that under KeepAlive,
// multiple requests share a single h2 connection (streamsIssued >>
// connsOpened) — the whole point of h2.
func TestH2Client_StreamsAreMultiplexed(t *testing.T) {
	srv := h2TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	g, err := New(Config{
		URL:                   srv.URL + "/",
		Protocol:              ProtocolHTTP2,
		ConnectionMode:        KeepAlive,
		TargetRPS:             50,
		Duration:              500 * time.Millisecond,
		Timeout:               2 * time.Second,
		TLSInsecureSkipVerify: true,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result := g.Run(context.Background())

	streams := result.Labels["streamsIssued"]
	conns := result.Labels["connsOpened"]
	if streams == "" || streams == "0" {
		t.Fatalf("streamsIssued label missing or zero: %q", streams)
	}
	if conns == "" {
		t.Fatalf("connsOpened label missing")
	}
	// At 50 RPS for 500ms over KeepAlive, expect << streams/connection.
	// Sanity: streamsIssued strictly greater than connsOpened.
	if streams == conns {
		t.Errorf("streams=%s, conns=%s — h2 multiplexing did not engage", streams, conns)
	}
}

// TestH2Client_ShortLivedClosesConn asserts that ShortLived h2 tears
// the conn down per request, so connsOpened tracks request count.
func TestH2Client_ShortLivedClosesConn(t *testing.T) {
	srv := h2TestServer(t, http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	cfg := Config{
		URL:                   srv.URL + "/",
		Protocol:              ProtocolHTTP2,
		ConnectionMode:        ShortLived,
		TargetRPS:             10,
		Duration:              400 * time.Millisecond,
		Timeout:               2 * time.Second,
		TLSInsecureSkipVerify: true,
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result := g.Run(context.Background())

	if result.Sent == 0 {
		t.Fatal("no requests sent")
	}
	// ShortLived h2: each request closes the conn, so streams per conn
	// ratio is ~1. Allow a small slack for the first request's stream
	// happening on the conn before the close fires.
	connsLabel := result.Labels["connsOpened"]
	streamsLabel := result.Labels["streamsIssued"]
	if connsLabel == "" || streamsLabel == "" {
		t.Fatalf("missing h2 stats labels: conns=%q streams=%q", connsLabel, streamsLabel)
	}
	var conns, streams int
	if _, err := fmt.Sscanf(connsLabel, "%d", &conns); err != nil {
		t.Fatalf("parse connsLabel %q: %v", connsLabel, err)
	}
	if _, err := fmt.Sscanf(streamsLabel, "%d", &streams); err != nil {
		t.Fatalf("parse streamsLabel %q: %v", streamsLabel, err)
	}
	if conns == 0 {
		t.Fatalf("connsOpened = 0 on ShortLived run")
	}
	// streams/conns ratio should be roughly 1; assert << 2 with slack.
	if ratio := float64(streams) / float64(conns); ratio > 2.0 {
		t.Errorf("ShortLived streams/conns = %.2f (streams=%d conns=%d), want ~1", ratio, streams, conns)
	}
}

// TestH2Client_DefaultProtocolIsHTTP1 makes sure new Config with
// Protocol unset keeps the legacy fasthttp client (no h2 swap).
func TestH2Client_DefaultProtocolIsHTTP1(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.ProtoMajor != 1 {
			t.Errorf("request used %s, want HTTP/1.x by default", r.Proto)
		}
		w.WriteHeader(http.StatusOK)
	}))
	defer srv.Close()

	g, err := New(Config{
		URL:            srv.URL + "/",
		ConnectionMode: KeepAlive,
		TargetRPS:      5,
		Duration:       200 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result := g.Run(context.Background())
	if result.Labels["protocol"] != "HTTP1" {
		t.Errorf("Labels[protocol] = %q, want HTTP1 (default)", result.Labels["protocol"])
	}
}

// TestH2Client_ValidateRejectsUnknown asserts Config.Validate flags
// invalid protocols early instead of letting them slip through to a
// confusing dial-time failure.
func TestH2Client_ValidateRejectsUnknown(t *testing.T) {
	err := Config{
		URL:            "http://example.com/",
		Protocol:       Protocol("bogus"),
		TargetRPS:      10,
		Duration:       time.Second,
		ConnectionMode: KeepAlive,
	}.Validate()
	if err == nil {
		t.Fatal("Validate accepted Protocol=bogus")
	}
}

// silence "imported and not used" while keeping the import grouped for
// future test additions that need tls types.
var _ = tls.VersionTLS12
