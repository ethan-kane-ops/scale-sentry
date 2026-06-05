package loadgen

import (
	"context"
	"fmt"
	"net"
	"testing"
	"time"

	"google.golang.org/grpc"
	"google.golang.org/grpc/health"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
)

// grpcHealthServer wires up a real grpc-go server with the Health
// service registered, listens on 127.0.0.1:0, and returns the address
// + a teardown the test can defer. Using the real server (not bufconn)
// keeps the test honest about actual h2 framing + dial costs.
func grpcHealthServer(t *testing.T, statuses map[string]healthpb.HealthCheckResponse_ServingStatus) string {
	t.Helper()
	lis, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatalf("listen: %v", err)
	}
	srv := grpc.NewServer()
	hs := health.NewServer()
	for svc, st := range statuses {
		hs.SetServingStatus(svc, st)
	}
	healthpb.RegisterHealthServer(srv, hs)
	go func() { _ = srv.Serve(lis) }()
	t.Cleanup(func() {
		srv.Stop()
		_ = lis.Close()
	})
	return lis.Addr().String()
}

// TestGRPCClient_HealthCheckSucceeds asserts a SERVING response is
// recorded as a 200 status and that the protocol label is GRPC.
func TestGRPCClient_HealthCheckSucceeds(t *testing.T) {
	addr := grpcHealthServer(t, map[string]healthpb.HealthCheckResponse_ServingStatus{
		"": healthpb.HealthCheckResponse_SERVING,
	})
	cfg := Config{
		URL:            "http://" + addr + "/",
		Protocol:       ProtocolGRPC,
		ConnectionMode: KeepAlive,
		TargetRPS:      20,
		Duration:       500 * time.Millisecond,
		Timeout:        2 * time.Second,
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result := g.Run(context.Background())
	if result.Sent == 0 {
		t.Fatal("no requests sent")
	}
	if result.Succeeded != result.Sent {
		t.Errorf("succeeded=%d sent=%d, want all succeeded; statusCounts=%v errors=%v",
			result.Succeeded, result.Sent, result.StatusCounts, result.Errors)
	}
	if got := result.Labels["protocol"]; got != "GRPC" {
		t.Errorf("Labels[protocol] = %q, want GRPC", got)
	}
	if result.StatusCounts[200] == 0 {
		t.Errorf("statusCounts[200] = 0, want >0; got %v", result.StatusCounts)
	}
}

// TestGRPCClient_NotServingMapsTo503 asserts the OK-status, NOT_SERVING
// case surfaces as a 503 so SLA verdicts still flag unhealthy targets.
func TestGRPCClient_NotServingMapsTo503(t *testing.T) {
	addr := grpcHealthServer(t, map[string]healthpb.HealthCheckResponse_ServingStatus{
		"": healthpb.HealthCheckResponse_NOT_SERVING,
	})
	cfg := Config{
		URL:            "http://" + addr + "/",
		Protocol:       ProtocolGRPC,
		ConnectionMode: KeepAlive,
		TargetRPS:      10,
		Duration:       300 * time.Millisecond,
		Timeout:        2 * time.Second,
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result := g.Run(context.Background())
	if result.Sent == 0 {
		t.Fatal("no requests sent")
	}
	if result.StatusCounts[503] == 0 {
		t.Errorf("NOT_SERVING did not produce 503; got statusCounts=%v", result.StatusCounts)
	}
}

// TestGRPCClient_PerServiceScoping asserts GRPCService is forwarded to
// the Check request: a service registered as SERVING returns 200, while
// an unknown service returns NotFound → 404.
func TestGRPCClient_PerServiceScoping(t *testing.T) {
	addr := grpcHealthServer(t, map[string]healthpb.HealthCheckResponse_ServingStatus{
		"my.Service": healthpb.HealthCheckResponse_SERVING,
	})
	// Sub-test for matched name.
	cfg := Config{
		URL:            "http://" + addr + "/",
		Protocol:       ProtocolGRPC,
		GRPCService:    "my.Service",
		ConnectionMode: KeepAlive,
		TargetRPS:      10,
		Duration:       300 * time.Millisecond,
		Timeout:        2 * time.Second,
	}
	g, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result := g.Run(context.Background())
	if result.StatusCounts[200] == 0 {
		t.Errorf("matched service: statusCounts[200]=0; want >0; got %v", result.StatusCounts)
	}
	if result.Labels["grpcService"] != "my.Service" {
		t.Errorf("Labels[grpcService]=%q, want my.Service", result.Labels["grpcService"])
	}

	// Unknown service: grpc-go's health server returns NotFound; the
	// classify path maps that to a non-zero status under our taxonomy.
	cfg.GRPCService = "no.Such.Service"
	g2, err := New(cfg)
	if err != nil {
		t.Fatalf("New (unknown svc): %v", err)
	}
	result = g2.Run(context.Background())
	if result.StatusCounts[404] == 0 {
		t.Errorf("unknown service: statusCounts[404]=0; got %v", result.StatusCounts)
	}
}

// TestGRPCClient_StreamsAreMultiplexed asserts that KeepAlive gRPC reuses
// a single subconn so streamsIssued >> connsOpened. Mirrors the h2 test.
func TestGRPCClient_StreamsAreMultiplexed(t *testing.T) {
	addr := grpcHealthServer(t, map[string]healthpb.HealthCheckResponse_ServingStatus{
		"": healthpb.HealthCheckResponse_SERVING,
	})
	g, err := New(Config{
		URL:            "http://" + addr + "/",
		Protocol:       ProtocolGRPC,
		ConnectionMode: KeepAlive,
		TargetRPS:      50,
		Duration:       500 * time.Millisecond,
		Timeout:        2 * time.Second,
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
	if streams == conns {
		t.Errorf("streams=%s, conns=%s — gRPC multiplexing did not engage", streams, conns)
	}
}

// TestGRPCClient_ShortLivedClosesConn asserts that ShortLived gRPC opens
// a fresh subconn per request. streams/conns ratio should be ~1.
func TestGRPCClient_ShortLivedClosesConn(t *testing.T) {
	addr := grpcHealthServer(t, map[string]healthpb.HealthCheckResponse_ServingStatus{
		"": healthpb.HealthCheckResponse_SERVING,
	})
	g, err := New(Config{
		URL:            "http://" + addr + "/",
		Protocol:       ProtocolGRPC,
		ConnectionMode: ShortLived,
		TargetRPS:      10,
		Duration:       400 * time.Millisecond,
		Timeout:        2 * time.Second,
	})
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	result := g.Run(context.Background())
	if result.Sent == 0 {
		t.Fatal("no requests sent")
	}
	connsLabel := result.Labels["connsOpened"]
	streamsLabel := result.Labels["streamsIssued"]
	var conns, streams int
	if _, err := fmt.Sscanf(connsLabel, "%d", &conns); err != nil {
		t.Fatalf("parse connsLabel %q: %v", connsLabel, err)
	}
	if _, err := fmt.Sscanf(streamsLabel, "%d", &streams); err != nil {
		t.Fatalf("parse streamsLabel %q: %v", streamsLabel, err)
	}
	if conns == 0 {
		t.Fatalf("connsOpened = 0 on ShortLived gRPC run")
	}
	if ratio := float64(streams) / float64(conns); ratio > 2.0 {
		t.Errorf("ShortLived streams/conns = %.2f (streams=%d conns=%d), want ~1", ratio, streams, conns)
	}
}

// TestGRPCClient_ValidateRejectsBadURL asserts a URL missing host:port
// is rejected at construction so misconfiguration surfaces immediately.
func TestGRPCClient_ValidateRejectsBadURL(t *testing.T) {
	_, err := newGRPCClient(Config{URL: "http:///nope", Protocol: ProtocolGRPC, ConnectionMode: KeepAlive, Timeout: time.Second})
	if err == nil {
		t.Fatal("newGRPCClient accepted url with no host")
	}
}
