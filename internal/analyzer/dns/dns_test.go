package dns

import (
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
)

func dnsConfig(ndots string) *corev1.PodDNSConfig {
	return &corev1.PodDNSConfig{
		Options: []corev1.PodDNSConfigOption{{Name: "ndots", Value: &ndots}},
	}
}

func TestAudit(t *testing.T) {
	tests := []struct {
		name         string
		cfg          *corev1.PodDNSConfig
		wantNDots    int
		wantExplicit bool
		wantErr      string
	}{
		{"nil config, k8s default", nil, 5, false, ""},
		{"config with no ndots option, k8s default", &corev1.PodDNSConfig{}, 5, false, ""},
		{"explicit ndots:1", dnsConfig("1"), 1, true, ""},
		{"explicit ndots:2", dnsConfig("2"), 2, true, ""},
		{"explicit ndots:5", dnsConfig("5"), 5, true, ""},
		{"non-numeric ndots", dnsConfig("five"), 0, false, "parse ndots value"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Audit(tc.cfg)
			if tc.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tc.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if r.NDots != tc.wantNDots {
				t.Errorf("NDots = %d, want %d", r.NDots, tc.wantNDots)
			}
			if r.Explicit != tc.wantExplicit {
				t.Errorf("Explicit = %v, want %v", r.Explicit, tc.wantExplicit)
			}
		})
	}
}

func TestAudit_NilOptionValue(t *testing.T) {
	cfg := &corev1.PodDNSConfig{Options: []corev1.PodDNSConfigOption{{Name: "ndots"}}}
	if _, err := Audit(cfg); err == nil {
		t.Fatal("expected error for ndots option with nil value")
	}
}

func TestDiagnostics(t *testing.T) {
	tests := []struct {
		name       string
		cfg        *corev1.PodDNSConfig
		wantAlerts int
	}{
		{"default ndots:5, warns", nil, 1},
		{"explicit ndots:5, warns", dnsConfig("5"), 1},
		{"explicit ndots:6, warns", dnsConfig("6"), 1},
		{"explicit ndots:2, clean", dnsConfig("2"), 0},
		{"explicit ndots:1, clean", dnsConfig("1"), 0},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Audit(tc.cfg)
			if err != nil {
				t.Fatalf("Audit: %v", err)
			}
			alerts := r.Diagnostics()
			if len(alerts) != tc.wantAlerts {
				t.Fatalf("alerts = %d, want %d (%+v)", len(alerts), tc.wantAlerts, alerts)
			}
			if tc.wantAlerts == 1 {
				if alerts[0].Type != "DNSNdotsHigh" {
					t.Errorf("Type = %q, want DNSNdotsHigh", alerts[0].Type)
				}
				if alerts[0].Severity != "Warning" {
					t.Errorf("Severity = %q, want Warning", alerts[0].Severity)
				}
			}
		})
	}
}
