package cgroup

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestParse(t *testing.T) {
	const body = `usage_usec 12345678
user_usec 9123456
system_usec 3222222
nr_periods 100
nr_throttled 7
throttled_usec 42000
core_sched.force_idle_usec 0
`
	s, err := Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	want := Stat{
		UsageUSec:     12345678,
		UserUSec:      9123456,
		SystemUSec:    3222222,
		NRPeriods:     100,
		NRThrottled:   7,
		ThrottledUSec: 42000,
	}
	if s != want {
		t.Errorf("Stat = %+v, want %+v", s, want)
	}
}

func TestParseTolerantOfBlanksAndExtraKeys(t *testing.T) {
	const body = `
nr_periods 50

nr_throttled 5
unknown_field 99
`
	s, err := Parse(strings.NewReader(body))
	if err != nil {
		t.Fatalf("Parse: %v", err)
	}
	if s.NRPeriods != 50 || s.NRThrottled != 5 {
		t.Errorf("Stat = %+v, want NRPeriods=50 NRThrottled=5", s)
	}
}

func TestParseRejectsNonNumeric(t *testing.T) {
	_, err := Parse(strings.NewReader("nr_periods banana\n"))
	if err == nil {
		t.Fatal("expected error for non-numeric value, got nil")
	}
	if !strings.Contains(err.Error(), "parse nr_periods") {
		t.Errorf("error %q does not mention nr_periods", err.Error())
	}
}

func TestParseFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cpu.stat")
	if err := os.WriteFile(path, []byte("nr_periods 10\nnr_throttled 1\n"), 0o600); err != nil {
		t.Fatalf("write fixture: %v", err)
	}
	s, err := ParseFile(path)
	if err != nil {
		t.Fatalf("ParseFile: %v", err)
	}
	if s.NRPeriods != 10 || s.NRThrottled != 1 {
		t.Errorf("Stat = %+v", s)
	}
}

func TestCompare(t *testing.T) {
	tests := []struct {
		name        string
		before      Stat
		after       Stat
		window      time.Duration
		wantPeriods uint64
		wantThrot   uint64
		wantPct     float64
		wantTime    time.Duration
	}{
		{
			name:        "no throttling",
			before:      Stat{NRPeriods: 100, NRThrottled: 0},
			after:       Stat{NRPeriods: 200, NRThrottled: 0},
			window:      time.Second,
			wantPeriods: 100,
			wantThrot:   0,
			wantPct:     0,
		},
		{
			name:        "10 pct throttling",
			before:      Stat{NRPeriods: 100, NRThrottled: 5, ThrottledUSec: 1_000_000},
			after:       Stat{NRPeriods: 200, NRThrottled: 15, ThrottledUSec: 3_000_000},
			window:      time.Second,
			wantPeriods: 100,
			wantThrot:   10,
			wantPct:     10.0,
			wantTime:    2 * time.Second,
		},
		{
			name:        "zero window — no periods elapsed",
			before:      Stat{NRPeriods: 100, NRThrottled: 5},
			after:       Stat{NRPeriods: 100, NRThrottled: 5},
			window:      time.Second,
			wantPeriods: 0,
			wantThrot:   0,
			wantPct:     0,
		},
		{
			name:        "counter rollback (defensive) — saturating sub yields 0",
			before:      Stat{NRPeriods: 200, NRThrottled: 10},
			after:       Stat{NRPeriods: 100, NRThrottled: 5},
			window:      time.Second,
			wantPeriods: 0,
			wantThrot:   0,
			wantPct:     0,
		},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r := Compare(tc.before, tc.after, tc.window)
			if r.Periods != tc.wantPeriods {
				t.Errorf("Periods = %d, want %d", r.Periods, tc.wantPeriods)
			}
			if r.Throttled != tc.wantThrot {
				t.Errorf("Throttled = %d, want %d", r.Throttled, tc.wantThrot)
			}
			if r.ThrottlePercent != tc.wantPct {
				t.Errorf("ThrottlePercent = %v, want %v", r.ThrottlePercent, tc.wantPct)
			}
			if r.ThrottledDuration != tc.wantTime {
				t.Errorf("ThrottledDuration = %v, want %v", r.ThrottledDuration, tc.wantTime)
			}
		})
	}
}

func TestDiagnosticsSeverityBands(t *testing.T) {
	tests := []struct {
		name         string
		periods      uint64
		throttled    uint64
		wantSeverity string
		wantAlerts   int
	}{
		{"no throttling — no alert", 100, 0, "", 0},
		{"info band — 1 pct", 100, 1, "Info", 1},
		{"warning band — 10 pct", 100, 10, "Warning", 1},
		{"critical band — 50 pct", 100, 50, "Critical", 1},
		{"exact warn threshold — 5 pct", 100, 5, "Warning", 1},
		{"exact critical threshold — 25 pct", 100, 25, "Critical", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			before := Stat{}
			after := Stat{NRPeriods: tc.periods, NRThrottled: tc.throttled, ThrottledUSec: 1000}
			alerts := Compare(before, after, time.Second).Diagnostics()
			if len(alerts) != tc.wantAlerts {
				t.Fatalf("got %d alerts, want %d", len(alerts), tc.wantAlerts)
			}
			if tc.wantAlerts == 0 {
				return
			}
			if alerts[0].Severity != tc.wantSeverity {
				t.Errorf("Severity = %q, want %q", alerts[0].Severity, tc.wantSeverity)
			}
			if alerts[0].Type != "CPUThrottling" {
				t.Errorf("Type = %q, want CPUThrottling", alerts[0].Type)
			}
		})
	}
}
