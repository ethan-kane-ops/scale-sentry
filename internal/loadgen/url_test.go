package loadgen

import (
	"strings"
	"testing"
)

func TestURLSpecBuild(t *testing.T) {
	tests := []struct {
		name    string
		spec    URLSpec
		want    string
		wantErr string
	}{
		{
			name: "ServiceDefault × ClusterIP defaults to /",
			spec: URLSpec{
				TargetMode:  TargetServiceDefault,
				NetworkPath: PathClusterIP,
				ServiceName: "checkout",
				Namespace:   "shop",
				Port:        8080,
			},
			want: "http://checkout.shop.svc.cluster.local:8080/",
		},
		{
			name: "CustomPath × ClusterIP keeps custom path",
			spec: URLSpec{
				TargetMode:  TargetCustomPath,
				NetworkPath: PathClusterIP,
				ServiceName: "api",
				Namespace:   "prod",
				CustomPath:  "/healthz/ready",
				Port:        80,
			},
			want: "http://api.prod.svc.cluster.local:80/healthz/ready",
		},
		{
			name: "AutoDiscoverProbe × ClusterIP requires customPath",
			spec: URLSpec{
				TargetMode:  TargetAutoDiscover,
				NetworkPath: PathClusterIP,
				ServiceName: "api",
				Namespace:   "prod",
				Port:        80,
			},
			wantErr: "customPath is required",
		},
		{
			name: "CustomPath × Ingress uses https by default",
			spec: URLSpec{
				TargetMode:  TargetCustomPath,
				NetworkPath: PathIngress,
				IngressHost: "checkout.example.com",
				CustomPath:  "/api/orders",
			},
			want: "https://checkout.example.com/api/orders",
		},
		{
			name: "Ingress with explicit port renders host:port",
			spec: URLSpec{
				TargetMode:  TargetServiceDefault,
				NetworkPath: PathIngress,
				IngressHost: "edge.example.com",
				Port:        8443,
			},
			want: "https://edge.example.com:8443/",
		},
		{
			name: "Scheme override is respected",
			spec: URLSpec{
				TargetMode:  TargetServiceDefault,
				NetworkPath: PathIngress,
				IngressHost: "edge.example.com",
				Scheme:      "http",
			},
			want: "http://edge.example.com/",
		},
		{
			name: "ClusterIP without service name errors",
			spec: URLSpec{
				TargetMode:  TargetServiceDefault,
				NetworkPath: PathClusterIP,
				Namespace:   "shop",
				Port:        80,
			},
			wantErr: "serviceName is required",
		},
		{
			name: "ClusterIP without port errors",
			spec: URLSpec{
				TargetMode:  TargetServiceDefault,
				NetworkPath: PathClusterIP,
				ServiceName: "svc",
				Namespace:   "ns",
			},
			wantErr: "port is required",
		},
		{
			name: "Ingress without host errors",
			spec: URLSpec{
				TargetMode:  TargetServiceDefault,
				NetworkPath: PathIngress,
			},
			wantErr: "ingressHost is required",
		},
		{
			name: "Missing networkPath errors",
			spec: URLSpec{
				TargetMode: TargetServiceDefault,
			},
			wantErr: "networkPath is required",
		},
		{
			name: "Missing targetMode errors",
			spec: URLSpec{
				NetworkPath: PathClusterIP,
				ServiceName: "svc",
				Namespace:   "ns",
				Port:        80,
			},
			wantErr: "targetMode is required",
		},
		{
			name: "CustomPath without leading slash errors",
			spec: URLSpec{
				TargetMode:  TargetCustomPath,
				NetworkPath: PathClusterIP,
				ServiceName: "svc",
				Namespace:   "ns",
				Port:        80,
				CustomPath:  "healthz",
			},
			wantErr: "must start with /",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := tc.spec.Build()
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got %q", tc.wantErr, got)
				}
				if !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.want {
				t.Fatalf("got %q, want %q", got, tc.want)
			}
		})
	}
}
