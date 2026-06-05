package loadgen

import (
	"strings"
	"testing"
	"time"
)

func TestPhase_Validate(t *testing.T) {
	t.Parallel()
	good := Phase{
		Name:        "m",
		Pattern:     PatternConstant,
		Duration:    time.Second,
		StartRPS:    10,
		RecordStats: true,
	}
	cases := []struct {
		name    string
		mutate  func(Phase) Phase
		wantErr string
	}{
		{"ok constant", func(p Phase) Phase { return p }, ""},
		{"missing name", func(p Phase) Phase { p.Name = ""; return p }, "name is required"},
		{"zero duration", func(p Phase) Phase { p.Duration = 0; return p }, "duration must be > 0"},
		{"negative duration", func(p Phase) Phase { p.Duration = -time.Second; return p }, "duration must be > 0"},
		{"zero startRPS", func(p Phase) Phase { p.StartRPS = 0; return p }, "startRPS must be > 0"},
		{"missing pattern", func(p Phase) Phase { p.Pattern = ""; return p }, "pattern is required"},
		{"unknown pattern", func(p Phase) Phase { p.Pattern = "Wat"; return p }, "unknown pattern"},
		{"ok poisson", func(p Phase) Phase { p.Pattern = PatternPoisson; return p }, ""},
		{"ramp needs endRPS", func(p Phase) Phase { p.Pattern = PatternRamp; return p }, "requires endRPS"},
		{"ok ramp", func(p Phase) Phase { p.Pattern = PatternRamp; p.EndRPS = 20; return p }, ""},
		{"step needs stepRPS", func(p Phase) Phase { p.Pattern = PatternStep; p.StepEvery = time.Second; return p }, "stepRPS"},
		{"step needs stepEvery", func(p Phase) Phase { p.Pattern = PatternStep; p.StepRPS = 5; return p }, "stepEvery"},
		{"ok step", func(p Phase) Phase {
			p.Pattern = PatternStep
			p.StepRPS = 5
			p.StepEvery = time.Second
			return p
		}, ""},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := tc.mutate(good).Validate()
			if tc.wantErr == "" {
				if err != nil {
					t.Errorf("want nil err, got %v", err)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), tc.wantErr) {
				t.Errorf("want err containing %q, got %v", tc.wantErr, err)
			}
		})
	}
}

func TestPhase_PeakRPS(t *testing.T) {
	t.Parallel()
	cases := []struct {
		name string
		p    Phase
		want int
	}{
		{
			"constant",
			Phase{Pattern: PatternConstant, StartRPS: 50},
			50,
		},
		{
			"poisson",
			Phase{Pattern: PatternPoisson, StartRPS: 75},
			75,
		},
		{
			"ramp ascending",
			Phase{Pattern: PatternRamp, StartRPS: 10, EndRPS: 200},
			200,
		},
		{
			"ramp descending falls back to start",
			Phase{Pattern: PatternRamp, StartRPS: 100, EndRPS: 25},
			100,
		},
		{
			"step plateau",
			Phase{Pattern: PatternStep, StartRPS: 10, StepRPS: 20, StepEvery: time.Second, Duration: 5 * time.Second},
			10 + 5*20,
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.p.peakRPS(); got != tc.want {
				t.Errorf("peakRPS = %d, want %d", got, tc.want)
			}
		})
	}
}
