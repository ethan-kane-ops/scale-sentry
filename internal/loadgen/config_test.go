package loadgen

import (
	"strings"
	"testing"
	"time"
)

func TestConfigValidate(t *testing.T) {
	base := Config{
		URL:            "http://example.com/",
		TargetRPS:      100,
		Duration:       10 * time.Second,
		ConnectionMode: KeepAlive,
	}

	tests := []struct {
		name    string
		mutate  func(*Config)
		wantErr string
	}{
		{"valid", func(c *Config) {}, ""},
		{"missing url", func(c *Config) { c.URL = "" }, "url is required"},
		{"bad scheme", func(c *Config) { c.URL = "ftp://x/" }, "scheme must be http or https"},
		{"missing host", func(c *Config) { c.URL = "http:///path" }, "url host is required"},
		{"zero rps", func(c *Config) { c.TargetRPS = 0 }, "targetRPS must be > 0"},
		{"negative rps", func(c *Config) { c.TargetRPS = -1 }, "targetRPS must be > 0"},
		{"zero duration", func(c *Config) { c.Duration = 0 }, "duration must be > 0"},
		{"missing connection mode", func(c *Config) { c.ConnectionMode = "" }, "connectionMode is required"},
		{"unknown connection mode", func(c *Config) { c.ConnectionMode = "Persistent" }, "unknown connectionMode"},
		{"valid short-lived", func(c *Config) { c.ConnectionMode = ShortLived }, ""},
		{"bad method", func(c *Config) { c.Method = "GET /bad" }, "invalid method"},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			cfg := base
			tc.mutate(&cfg)
			err := cfg.Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				return
			}
			if err == nil {
				t.Fatalf("expected error containing %q, got nil", tc.wantErr)
			}
			if !strings.Contains(err.Error(), tc.wantErr) {
				t.Fatalf("error %q does not contain %q", err.Error(), tc.wantErr)
			}
		})
	}
}

func TestConfigWithDefaults(t *testing.T) {
	cfg := Config{
		URL:            "http://example.com/",
		TargetRPS:      50,
		Duration:       time.Second,
		ConnectionMode: KeepAlive,
	}
	got := cfg.withDefaults()
	if got.Method != "GET" {
		t.Errorf("Method = %q, want GET", got.Method)
	}
	if got.Timeout != 5*time.Second {
		t.Errorf("Timeout = %v, want 5s", got.Timeout)
	}
	if got.Concurrency != 50 {
		t.Errorf("Concurrency = %d, want 50", got.Concurrency)
	}

	cfg.TargetRPS = 10_000
	got = cfg.withDefaults()
	if got.Concurrency != 256 {
		t.Errorf("Concurrency cap = %d, want 256", got.Concurrency)
	}
}
