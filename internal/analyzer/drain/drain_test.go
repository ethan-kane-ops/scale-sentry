package drain

import (
	"testing"
	"time"

	"github.com/ethan-kane-ops/scale-sentry/internal/analyzer/leakage"
)

func t0() time.Time { return time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC) }

func TestCorrelate_DroppedAndClean(t *testing.T) {
	start := t0()
	events := []leakage.EndpointEvent{
		{At: start.Add(10 * time.Second), PodIP: "10.0.0.1", Kind: leakage.EndpointRemoved},
	}
	errors := []leakage.ErrorSample{
		{At: start.Add(10*time.Second + 500*time.Millisecond), Category: "ConnReset"}, // dropped
		{At: start.Add(12 * time.Second), Category: "Server5xx", Status: 502},         // dropped
		{At: start.Add(25 * time.Second), Category: "Server5xx", Status: 500},         // clean (outside 10s window)
		{At: start.Add(5 * time.Second), Category: "Dial"},                            // clean (before removal)
	}

	r := Correlate(events, errors, 0) // default 10s window
	if r.DroppedRequests != 2 {
		t.Errorf("DroppedRequests = %d, want 2", r.DroppedRequests)
	}
	if r.CleanRequests != 2 {
		t.Errorf("CleanRequests = %d, want 2", r.CleanRequests)
	}
	if r.DrainWindow != DefaultDrainWindow {
		t.Errorf("DrainWindow = %v, want default %v", r.DrainWindow, DefaultDrainWindow)
	}
	if r.RemovalCount != 1 {
		t.Errorf("RemovalCount = %d, want 1", r.RemovalCount)
	}
	if len(r.Correlated) != 1 || len(r.Correlated[0].Errors) != 2 {
		t.Errorf("Correlated = %+v, want one removal with 2 errors", r.Correlated)
	}
}

func TestCorrelate_LookbackToleratesInformerLag(t *testing.T) {
	start := t0()
	events := []leakage.EndpointEvent{
		{At: start.Add(10 * time.Second), PodIP: "10.0.0.1", Kind: leakage.EndpointRemoved},
	}
	errors := []leakage.ErrorSample{
		{At: start.Add(10*time.Second - 500*time.Millisecond), Category: "ConnReset"}, // dropped: within lookback
		{At: start.Add(10*time.Second - DefaultLookback), Category: "ConnReset"},      // dropped: exactly at the boundary
		{At: start.Add(5 * time.Second), Category: "Dial"},                            // clean: well before lookback
	}

	r := Correlate(events, errors, 0)
	if r.DroppedRequests != 2 {
		t.Errorf("DroppedRequests = %d, want 2", r.DroppedRequests)
	}
	if r.CleanRequests != 1 {
		t.Errorf("CleanRequests = %d, want 1", r.CleanRequests)
	}
}

func TestCorrelate_IgnoresReadyEvents(t *testing.T) {
	start := t0()
	events := []leakage.EndpointEvent{
		{At: start, PodIP: "10.0.0.1", Kind: leakage.EndpointReady},
	}
	errors := []leakage.ErrorSample{
		{At: start.Add(time.Second), Category: "ConnReset"},
	}
	r := Correlate(events, errors, 5*time.Second)
	if r.DroppedRequests != 0 {
		t.Errorf("DroppedRequests = %d, want 0 (only Removed events count)", r.DroppedRequests)
	}
	if r.CleanRequests != 1 {
		t.Errorf("CleanRequests = %d, want 1", r.CleanRequests)
	}
	if r.RemovalCount != 0 {
		t.Errorf("RemovalCount = %d, want 0", r.RemovalCount)
	}
}

func TestCorrelate_NoRemovals_AllClean(t *testing.T) {
	start := t0()
	errors := []leakage.ErrorSample{
		{At: start, Category: "Server5xx", Status: 500},
		{At: start.Add(time.Second), Category: "ConnReset"},
	}
	r := Correlate(nil, errors, 5*time.Second)
	if r.DroppedRequests != 0 || r.CleanRequests != 2 {
		t.Errorf("Dropped=%d Clean=%d, want 0/2", r.DroppedRequests, r.CleanRequests)
	}
}

func TestCorrelate_SortsUnorderedInput(t *testing.T) {
	start := t0()
	events := []leakage.EndpointEvent{
		{At: start.Add(20 * time.Second), PodIP: "10.0.0.2", Kind: leakage.EndpointRemoved},
		{At: start.Add(10 * time.Second), PodIP: "10.0.0.1", Kind: leakage.EndpointRemoved},
	}
	errors := []leakage.ErrorSample{
		{At: start.Add(20500 * time.Millisecond), Category: "ConnReset"},
		{At: start.Add(10500 * time.Millisecond), Category: "Server5xx", Status: 502},
	}
	r := Correlate(events, errors, 5*time.Second)
	if r.DroppedRequests != 2 {
		t.Errorf("DroppedRequests = %d, want 2", r.DroppedRequests)
	}
	if !r.Correlated[0].Event.At.Before(r.Correlated[1].Event.At) {
		t.Errorf("Correlated not sorted by removal time: %+v", r.Correlated)
	}
}

func TestDiagnostics_SeverityBands(t *testing.T) {
	start := t0()
	build := func(dropped int) Report {
		events := []leakage.EndpointEvent{{At: start, PodIP: "10.0.0.1", Kind: leakage.EndpointRemoved}}
		errors := make([]leakage.ErrorSample, dropped)
		for i := range errors {
			errors[i] = leakage.ErrorSample{At: start.Add(time.Duration(i) * time.Millisecond), Category: "ConnReset"}
		}
		return Correlate(events, errors, 10*time.Second)
	}

	tests := []struct {
		name         string
		dropped      int
		wantSeverity string
		wantAlerts   int
	}{
		{"no drops", 0, "", 0},
		{"single drop, Warning", 1, "Warning", 1},
		{"below critical threshold", 24, "Warning", 1},
		{"at critical threshold", 25, "Critical", 1},
		{"well over critical", 100, "Critical", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			alerts := build(tc.dropped).Diagnostics()
			if len(alerts) != tc.wantAlerts {
				t.Fatalf("alerts = %d, want %d", len(alerts), tc.wantAlerts)
			}
			if tc.wantAlerts == 0 {
				return
			}
			if alerts[0].Severity != tc.wantSeverity {
				t.Errorf("Severity = %q, want %q", alerts[0].Severity, tc.wantSeverity)
			}
			if alerts[0].Type != "UngracefulDrain" {
				t.Errorf("Type = %q, want UngracefulDrain", alerts[0].Type)
			}
		})
	}
}
