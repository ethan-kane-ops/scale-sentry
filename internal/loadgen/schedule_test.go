package loadgen

import (
	"context"
	"testing"
	"time"
)

// collectSchedule drains runSchedule for a phase into a slice. Used to
// assert per-pattern emission shape without standing up the full Generator.
func collectSchedule(phase Phase) []scheduledArrival {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	out := make(chan scheduledArrival, 1024)
	go runSchedule(ctx, time.Now(), phase, out)
	var got []scheduledArrival
	for s := range out {
		got = append(got, s)
	}
	return got
}

func TestRunSchedule_Constant(t *testing.T) {
	t.Parallel()
	got := collectSchedule(Phase{
		Name:     "c",
		Pattern:  PatternConstant,
		Duration: 100 * time.Millisecond,
		StartRPS: 100, // ~10 arrivals over 100ms
	})
	if len(got) == 0 {
		t.Fatal("constant emitter produced zero arrivals")
	}
	// Inter-arrival should be ~10ms; check the first delta is bounded.
	if len(got) >= 2 {
		delta := got[1].Time.Sub(got[0].Time)
		if delta < 5*time.Millisecond || delta > 20*time.Millisecond {
			t.Errorf("constant delta = %v, want ~10ms", delta)
		}
	}
}

func TestRunSchedule_Poisson(t *testing.T) {
	t.Parallel()
	got := collectSchedule(Phase{
		Name:     "p",
		Pattern:  PatternPoisson,
		Duration: 200 * time.Millisecond,
		StartRPS: 500,
	})
	if len(got) < 10 {
		t.Errorf("Poisson @ 500rps over 200ms produced %d arrivals, want >=10", len(got))
	}
}

func TestRunSchedule_Ramp(t *testing.T) {
	t.Parallel()
	got := collectSchedule(Phase{
		Name:     "r",
		Pattern:  PatternRamp,
		Duration: 200 * time.Millisecond,
		StartRPS: 10,
		EndRPS:   1000, // accelerating
	})
	if len(got) == 0 {
		t.Fatal("ramp emitter produced zero arrivals")
	}
	// Ramp accelerates: late inter-arrivals should be shorter than early ones.
	if len(got) >= 4 {
		early := got[1].Time.Sub(got[0].Time)
		late := got[len(got)-1].Time.Sub(got[len(got)-2].Time)
		if late >= early {
			t.Errorf("ramp not accelerating: early=%v late=%v", early, late)
		}
	}
}

func TestRunSchedule_Step(t *testing.T) {
	t.Parallel()
	got := collectSchedule(Phase{
		Name:      "s",
		Pattern:   PatternStep,
		Duration:  200 * time.Millisecond,
		StartRPS:  50,
		StepRPS:   100,
		StepEvery: 100 * time.Millisecond,
	})
	if len(got) == 0 {
		t.Fatal("step emitter produced zero arrivals")
	}
}

func TestRunSchedule_RampDescendingClampsRate(t *testing.T) {
	t.Parallel()
	// Ramp from very small to even smaller, the floor=1 branch in
	// emitRamp keeps the schedule producing tokens instead of stalling.
	got := collectSchedule(Phase{
		Name:     "r",
		Pattern:  PatternRamp,
		Duration: 100 * time.Millisecond,
		StartRPS: 2,
		EndRPS:   1,
	})
	if len(got) == 0 {
		t.Fatal("ramp floor=1 path produced zero arrivals")
	}
}

func TestRunSchedule_ContextCancelStopsEmission(t *testing.T) {
	t.Parallel()
	ctx, cancel := context.WithCancel(context.Background())
	out := make(chan scheduledArrival, 4)
	go runSchedule(ctx, time.Now(), Phase{
		Name:     "c",
		Pattern:  PatternConstant,
		Duration: 5 * time.Second,
		StartRPS: 10,
	}, out)
	// Read one, cancel, then drain. Must terminate cleanly.
	<-out
	cancel()
	for range out {
	}
}
