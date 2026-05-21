package leakage

import (
	"testing"
	"time"
)

func t0() time.Time { return time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC) }

func TestCorrelate_LeakedAndCleanCategorisation(t *testing.T) {
	start := t0()
	events := []EndpointEvent{
		{At: start.Add(10 * time.Second), PodIP: "10.0.0.1", Kind: EndpointReady},
	}
	errors := []ErrorSample{
		{At: start.Add(10*time.Second + 500*time.Millisecond), Category: "Server5xx", Status: 503}, // leaked
		{At: start.Add(11 * time.Second), Category: "ConnReset"},                                   // leaked
		{At: start.Add(15 * time.Second), Category: "Server5xx", Status: 500},                      // clean (outside 2s window)
		{At: start.Add(5 * time.Second), Category: "Dial"},                                         // clean (before event)
	}

	r := Correlate(events, errors, 0) // default 2s window
	if r.LeakedRequests != 2 {
		t.Errorf("LeakedRequests = %d, want 2", r.LeakedRequests)
	}
	if r.CleanRequests != 2 {
		t.Errorf("CleanRequests = %d, want 2", r.CleanRequests)
	}
	if r.LeakageWindow != DefaultLeakageWindow {
		t.Errorf("LeakageWindow = %v, want default %v", r.LeakageWindow, DefaultLeakageWindow)
	}
	if r.EndpointEventCount != 1 {
		t.Errorf("EndpointEventCount = %d, want 1", r.EndpointEventCount)
	}
	if len(r.Correlated) != 1 || len(r.Correlated[0].Errors) != 2 {
		t.Errorf("Correlated = %+v, want one event with 2 errors", r.Correlated)
	}
}

func TestCorrelate_NoReadyEvents_AllClean(t *testing.T) {
	start := t0()
	errors := []ErrorSample{
		{At: start, Category: "Server5xx", Status: 500},
		{At: start.Add(1 * time.Second), Category: "ConnReset"},
	}
	r := Correlate(nil, errors, 2*time.Second)
	if r.LeakedRequests != 0 {
		t.Errorf("LeakedRequests = %d, want 0", r.LeakedRequests)
	}
	if r.CleanRequests != 2 {
		t.Errorf("CleanRequests = %d, want 2", r.CleanRequests)
	}
}

func TestCorrelate_IgnoresRemovedEvents(t *testing.T) {
	start := t0()
	events := []EndpointEvent{
		{At: start, PodIP: "10.0.0.1", Kind: EndpointRemoved},
	}
	errors := []ErrorSample{
		{At: start.Add(500 * time.Millisecond), Category: "Server5xx", Status: 503},
	}
	r := Correlate(events, errors, 2*time.Second)
	if r.LeakedRequests != 0 {
		t.Errorf("LeakedRequests = %d, want 0 (only Ready events count)", r.LeakedRequests)
	}
	if r.CleanRequests != 1 {
		t.Errorf("CleanRequests = %d, want 1", r.CleanRequests)
	}
	if r.EndpointEventCount != 0 {
		t.Errorf("EndpointEventCount = %d, want 0 (Removed excluded)", r.EndpointEventCount)
	}
}

func TestCorrelate_AssignsToFirstMatchingEvent(t *testing.T) {
	start := t0()
	events := []EndpointEvent{
		{At: start.Add(10 * time.Second), PodIP: "10.0.0.1", Kind: EndpointReady},
		{At: start.Add(11 * time.Second), PodIP: "10.0.0.2", Kind: EndpointReady},
	}
	errors := []ErrorSample{
		// Falls inside both windows (10–12s and 11–13s); should attach to the
		// first event chronologically (10s).
		{At: start.Add(11500 * time.Millisecond), Category: "Server5xx", Status: 503},
	}
	r := Correlate(events, errors, 2*time.Second)
	if r.LeakedRequests != 1 {
		t.Fatalf("LeakedRequests = %d, want 1", r.LeakedRequests)
	}
	if len(r.Correlated[0].Errors) != 1 {
		t.Errorf("first event errors = %d, want 1", len(r.Correlated[0].Errors))
	}
	if len(r.Correlated[1].Errors) != 0 {
		t.Errorf("second event errors = %d, want 0 (already claimed by first)", len(r.Correlated[1].Errors))
	}
}

func TestCorrelate_SortsUnorderedInput(t *testing.T) {
	start := t0()
	events := []EndpointEvent{
		{At: start.Add(20 * time.Second), PodIP: "10.0.0.2", Kind: EndpointReady},
		{At: start.Add(10 * time.Second), PodIP: "10.0.0.1", Kind: EndpointReady},
	}
	errors := []ErrorSample{
		{At: start.Add(20500 * time.Millisecond), Category: "ConnReset"},
		{At: start.Add(10500 * time.Millisecond), Category: "Server5xx", Status: 503},
	}
	r := Correlate(events, errors, 2*time.Second)
	if r.LeakedRequests != 2 {
		t.Errorf("LeakedRequests = %d, want 2", r.LeakedRequests)
	}
	// Correlated should be sorted by event time.
	if !r.Correlated[0].Event.At.Before(r.Correlated[1].Event.At) {
		t.Errorf("Correlated not sorted by event time: %+v", r.Correlated)
	}
}

func TestDiagnostics_SeverityBands(t *testing.T) {
	start := t0()
	build := func(leaked int) Report {
		events := []EndpointEvent{{At: start, PodIP: "10.0.0.1", Kind: EndpointReady}}
		errors := make([]ErrorSample, leaked)
		for i := range errors {
			errors[i] = ErrorSample{At: start.Add(time.Duration(i) * time.Millisecond), Category: "Server5xx", Status: 503}
		}
		return Correlate(events, errors, 2*time.Second)
	}

	tests := []struct {
		name         string
		leaked       int
		wantSeverity string
		wantAlerts   int
	}{
		{"no leaks", 0, "", 0},
		{"single leak — Warning", 1, "Warning", 1},
		{"below critical threshold", 49, "Warning", 1},
		{"at critical threshold", 50, "Critical", 1},
		{"well over critical", 200, "Critical", 1},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			alerts := build(tc.leaked).Diagnostics()
			if len(alerts) != tc.wantAlerts {
				t.Fatalf("got %d alerts, want %d", len(alerts), tc.wantAlerts)
			}
			if tc.wantAlerts == 0 {
				return
			}
			if alerts[0].Severity != tc.wantSeverity {
				t.Errorf("Severity = %q, want %q", alerts[0].Severity, tc.wantSeverity)
			}
			if alerts[0].Type != "ColdStartLeakage" {
				t.Errorf("Type = %q, want ColdStartLeakage", alerts[0].Type)
			}
		})
	}
}
