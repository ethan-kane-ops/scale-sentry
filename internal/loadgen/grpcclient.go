package loadgen

import (
	"context"
	"fmt"
	"net/url"
	"strings"
	"sync"

	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/credentials/insecure"
	healthpb "google.golang.org/grpc/health/grpc_health_v1"
	"google.golang.org/grpc/stats"
	"google.golang.org/grpc/status"
)

// grpcClient dispatches each request as a grpc.health.v1.Health/Check
// unary call against the target's host:port. The path component of cfg.URL
// is ignored: gRPC does not use it. https:// URLs dial over TLS using the
// loadgen TLS config; http:// URLs dial cleartext.
//
// The Health probe was chosen deliberately for scope reasons. The goal of
// gRPC support in loadgen is to drive realistic h2-framed traffic at the
// target so HPAs scale on it; arbitrary RPC method invocation (with
// reflection or compiled stubs) is meaningful product surface but not
// load-shape surface. A future ticket can layer method invocation on top
// without disturbing the protocol switch wired here.
//
// Status mapping: gRPC status codes are mapped to nearest HTTP analogues
// so the loadgen histogram, statusCounts, and SLA verdict stay
// protocol-agnostic. A NOT_SERVING Health response is returned as 503
// (the request succeeded; the target reported itself unhealthy).
type grpcClient struct {
	cfg     Config
	target  string
	useTLS  bool
	creds   credentials.TransportCredentials
	tracker grpcStatsTracker

	keepaliveOnce sync.Once
	keepaliveConn *grpc.ClientConn
	keepaliveErr  error
}

// grpcStatsTracker counts conn and stream events fed by grpc-go's
// stats.Handler hooks. All counters are atomic so the post-run snapshot
// is consistent without locking the hot path.
type grpcStatsTracker struct {
	conns   atomicCounter
	streams atomicCounter
}

func (t *grpcStatsTracker) TagRPC(ctx context.Context, _ *stats.RPCTagInfo) context.Context {
	return ctx
}
func (t *grpcStatsTracker) HandleRPC(_ context.Context, rs stats.RPCStats) {
	if _, ok := rs.(*stats.Begin); ok {
		t.streams.Add(1)
	}
}
func (t *grpcStatsTracker) TagConn(ctx context.Context, _ *stats.ConnTagInfo) context.Context {
	return ctx
}
func (t *grpcStatsTracker) HandleConn(_ context.Context, cs stats.ConnStats) {
	if _, ok := cs.(*stats.ConnBegin); ok {
		t.conns.Add(1)
	}
}

// newGRPCClient builds the gRPC dispatcher. The dial itself is deferred
// (grpc.NewClient is non-blocking and lazily establishes subconns on the
// first RPC) so newGRPCClient never blocks on a misconfigured target;
// dial failures surface as RPC errors at Do() time and flow into the
// loadgen error taxonomy alongside HTTP transport errors.
func newGRPCClient(cfg Config) (*grpcClient, error) {
	u, err := url.Parse(cfg.URL)
	if err != nil {
		return nil, fmt.Errorf("parse url: %w", err)
	}
	if u.Host == "" {
		return nil, fmt.Errorf("grpc target missing host:port in url %q", cfg.URL)
	}
	useTLS := strings.EqualFold(u.Scheme, "https")
	var creds credentials.TransportCredentials
	if useTLS {
		tlsCfg, err := buildTLSConfig(cfg)
		if err != nil {
			return nil, err
		}
		creds = credentials.NewTLS(tlsCfg)
	} else {
		creds = insecure.NewCredentials()
	}
	return &grpcClient{
		cfg:    cfg,
		target: u.Host,
		useTLS: useTLS,
		creds:  creds,
	}, nil
}

// dial constructs a fresh ClientConn. Used directly by ShortLived runs
// (one conn per request) and lazily once by KeepAlive runs.
func (g *grpcClient) dial() (*grpc.ClientConn, error) {
	return grpc.NewClient(g.target,
		grpc.WithTransportCredentials(g.creds),
		grpc.WithStatsHandler(&g.tracker),
	)
}

// Do issues one Health/Check RPC. Returns a fake HTTP status code so the
// generator's collector can categorize the response on the same axis as
// HTTP runs (2xx = success, 5xx = server-side failure, etc.). Transport
// dial errors return (0, err) so classify() routes them through the
// network-error taxonomy instead of pretending we got a real response.
func (g *grpcClient) Do(ctx context.Context) (int, error) {
	ctx, cancel := context.WithTimeout(ctx, g.cfg.Timeout)
	defer cancel()

	var conn *grpc.ClientConn
	if g.cfg.ConnectionMode == ShortLived {
		c, err := g.dial()
		if err != nil {
			return 0, fmt.Errorf("grpc dial: %w", err)
		}
		defer func() { _ = c.Close() }()
		conn = c
	} else {
		g.keepaliveOnce.Do(func() {
			g.keepaliveConn, g.keepaliveErr = g.dial()
		})
		if g.keepaliveErr != nil {
			return 0, g.keepaliveErr
		}
		conn = g.keepaliveConn
	}

	client := healthpb.NewHealthClient(conn)
	resp, err := client.Check(ctx, &healthpb.HealthCheckRequest{Service: g.cfg.GRPCService})
	if err != nil {
		if st, ok := status.FromError(err); ok {
			// gRPC status mapping: server replied with a non-OK code.
			// Return the mapped HTTP equivalent + nil err so the
			// collector tags it via statusCounts (the same path 4xx/5xx
			// HTTP responses take) instead of the transport-error path.
			return grpcStatusToHTTP(st.Code()), nil
		}
		return 0, err
	}
	if resp.GetStatus() != healthpb.HealthCheckResponse_SERVING {
		// OK status but target reports itself unhealthy. Surface as 503
		// so SLA verdicts still flag the run as failing while keeping
		// the histogram protocol-agnostic.
		return 503, nil
	}
	return 200, nil
}

// Stats reports the live gRPC subconn and stream counters. GoAwayCount is
// always 0: grpc-go handles GOAWAY transparently by tearing the subconn
// down and reconnecting, so the observable signal is elevated ConnsOpened,
// not a dedicated counter. Documented in package doc.
func (g *grpcClient) Stats() ClientStats {
	return ClientStats{
		ConnsOpened:   g.tracker.conns.Load(),
		StreamsIssued: g.tracker.streams.Load(),
	}
}

// grpcStatusToHTTP maps a gRPC status.Code to its nearest HTTP analogue.
// The mapping follows google.api.Code annotations used by gRPC gateway
// and grpc-gateway projects. The goal is histogram parity with the HTTP
// clients: a server error on either wire format buckets the same way.
func grpcStatusToHTTP(c codes.Code) int {
	switch c {
	case codes.OK:
		return 200
	case codes.Canceled:
		return 499
	case codes.InvalidArgument, codes.OutOfRange:
		return 400
	case codes.Unauthenticated:
		return 401
	case codes.PermissionDenied:
		return 403
	case codes.NotFound:
		return 404
	case codes.AlreadyExists, codes.Aborted:
		return 409
	case codes.FailedPrecondition:
		return 412
	case codes.ResourceExhausted:
		return 429
	case codes.Internal, codes.DataLoss, codes.Unknown:
		return 500
	case codes.Unimplemented:
		return 501
	case codes.Unavailable:
		return 503
	case codes.DeadlineExceeded:
		return 504
	default:
		return 500
	}
}

// Compile-time guard: grpcClient must satisfy the protocolClient contract.
var _ protocolClient = (*grpcClient)(nil)
