package probe

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func numericPort(port int32, path, scheme string) corev1.Container {
	return corev1.Container{
		ReadinessProbe: &corev1.Probe{
			ProbeHandler: corev1.ProbeHandler{
				HTTPGet: &corev1.HTTPGetAction{
					Path:   path,
					Port:   intstr.FromInt32(port),
					Scheme: corev1.URIScheme(scheme),
				},
			},
		},
	}
}

func TestDiscoverFromContainer(t *testing.T) {
	tests := []struct {
		name      string
		container corev1.Container
		want      Spec
		wantErr   string
	}{
		{
			name:      "numeric port with path",
			container: numericPort(8080, "/healthz/ready", "HTTP"),
			want:      Spec{Path: "/healthz/ready", Port: 8080, Scheme: "HTTP"},
		},
		{
			name:      "empty path defaults to /",
			container: numericPort(8080, "", ""),
			want:      Spec{Path: "/", Port: 8080, Scheme: "HTTP"},
		},
		{
			name:      "HTTPS scheme preserved",
			container: numericPort(8443, "/health", "HTTPS"),
			want:      Spec{Path: "/health", Port: 8443, Scheme: "HTTPS"},
		},
		{
			name: "named port resolved against container.ports",
			container: corev1.Container{
				Ports: []corev1.ContainerPort{
					{Name: "http", ContainerPort: 8080},
					{Name: "metrics", ContainerPort: 9090},
				},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: "/ready",
							Port: intstr.FromString("http"),
						},
					},
				},
			},
			want: Spec{Path: "/ready", Port: 8080, Scheme: "HTTP"},
		},
		{
			name: "Host override preserved",
			container: corev1.Container{
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: "/ready",
							Port: intstr.FromInt32(8080),
							Host: "internal.local",
						},
					},
				},
			},
			want: Spec{Path: "/ready", Port: 8080, Scheme: "HTTP", Host: "internal.local"},
		},
		{
			name:      "missing readiness probe",
			container: corev1.Container{},
			wantErr:   "no readinessProbe",
		},
		{
			name: "non-HTTP probe (tcpSocket)",
			container: corev1.Container{
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(8080)},
					},
				},
			},
			wantErr: "not HTTPGet",
		},
		{
			name: "named port not in container.ports",
			container: corev1.Container{
				Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 8080}},
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: "/ready",
							Port: intstr.FromString("api"),
						},
					},
				},
			},
			wantErr: `named port "api" not found`,
		},
		{
			name: "numeric port zero",
			container: corev1.Container{
				ReadinessProbe: &corev1.Probe{
					ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: "/ready",
							Port: intstr.FromInt32(0),
						},
					},
				},
			},
			wantErr: "must be > 0",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got, err := DiscoverFromContainer(tc.container)
			if tc.wantErr != "" {
				if err == nil {
					t.Fatalf("expected error containing %q, got %+v", tc.wantErr, got)
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
				t.Fatalf("got %+v, want %+v", got, tc.want)
			}
		})
	}
}
