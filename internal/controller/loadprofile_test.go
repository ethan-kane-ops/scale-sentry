package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1beta1 "github.com/ethan-kane-ops/scale-sentry/api/v1beta1"
	"github.com/ethan-kane-ops/scale-sentry/internal/loadgen"
)

func crWithLoad(load v1beta1.LoadConfig, sla time.Duration) *v1beta1.ScaleValidation {
	return &v1beta1.ScaleValidation{
		Spec: v1beta1.ScaleValidationSpec{
			SLA:  metav1.Duration{Duration: sla},
			Load: load,
		},
	}
}

func TestBuildPhases_NoProfileNoWarmupReturnsNil(t *testing.T) {
	cr := crWithLoad(v1beta1.LoadConfig{BaseRPS: 100}, time.Minute)
	phases, err := buildPhases(cr)
	if err != nil {
		t.Fatalf("buildPhases: %v", err)
	}
	if phases != nil {
		t.Errorf("phases = %+v, want nil (legacy single-shot path)", phases)
	}
}

func TestBuildPhases_WarmupPrependsRecordStatsFalsePhase(t *testing.T) {
	cr := crWithLoad(v1beta1.LoadConfig{
		BaseRPS:        100,
		WarmupDuration: &metav1.Duration{Duration: 10 * time.Second},
	}, time.Minute)
	phases, err := buildPhases(cr)
	if err != nil {
		t.Fatalf("buildPhases: %v", err)
	}
	if len(phases) != 2 {
		t.Fatalf("phases len = %d, want 2 (warmup + measure)", len(phases))
	}
	if phases[0].Name != loadgen.WarmupPhaseName || phases[0].RecordStats {
		t.Errorf("phases[0] = %+v, want Warmup w/ RecordStats=false", phases[0])
	}
	if phases[0].Duration != 10*time.Second {
		t.Errorf("phases[0].Duration = %v, want 10s", phases[0].Duration)
	}
	if phases[1].Duration != 50*time.Second {
		t.Errorf("phases[1].Duration = %v, want SLA - warmup = 50s", phases[1].Duration)
	}
}

func TestBuildPhases_RampRequiresEndRPS(t *testing.T) {
	cr := crWithLoad(v1beta1.LoadConfig{
		BaseRPS: 50,
		Profile: &v1beta1.LoadProfile{Pattern: "Ramp"},
	}, time.Minute)
	if _, err := buildPhases(cr); err == nil {
		t.Error("expected error when Ramp pattern has no endRps")
	}
}

func TestBuildPhases_RampOK(t *testing.T) {
	end := int32(200)
	cr := crWithLoad(v1beta1.LoadConfig{
		BaseRPS: 50,
		Profile: &v1beta1.LoadProfile{Pattern: "Ramp", EndRPS: &end},
	}, time.Minute)
	phases, err := buildPhases(cr)
	if err != nil {
		t.Fatalf("buildPhases: %v", err)
	}
	if len(phases) != 1 || phases[0].Pattern != loadgen.PatternRamp || phases[0].EndRPS != 200 {
		t.Errorf("phases = %+v, want one Ramp phase with EndRPS=200", phases)
	}
}

func TestBuildPhases_Spike(t *testing.T) {
	cr := crWithLoad(v1beta1.LoadConfig{
		BaseRPS: 50,
		Profile: &v1beta1.LoadProfile{
			Pattern: "Spike",
			Spikes: []v1beta1.SpikeWindow{
				{At: metav1.Duration{Duration: 10 * time.Second}, Duration: metav1.Duration{Duration: 5 * time.Second}, RPS: 200},
			},
		},
	}, time.Minute)
	phases, err := buildPhases(cr)
	if err != nil {
		t.Fatalf("buildPhases: %v", err)
	}
	// Expect base(0..10s), spike(10..15s), tail(15..60s) = 3 phases.
	if len(phases) != 3 {
		t.Fatalf("phases len = %d, want 3 (base + spike + tail)", len(phases))
	}
	if phases[1].StartRPS != 200 {
		t.Errorf("phases[1].StartRPS = %d, want 200 (spike)", phases[1].StartRPS)
	}
}

func TestBuildPhases_SpikeBelowBaseIsRejected(t *testing.T) {
	cr := crWithLoad(v1beta1.LoadConfig{
		BaseRPS: 100,
		Profile: &v1beta1.LoadProfile{
			Pattern: "Spike",
			Spikes: []v1beta1.SpikeWindow{
				{At: metav1.Duration{Duration: 5 * time.Second}, Duration: metav1.Duration{Duration: 5 * time.Second}, RPS: 50},
			},
		},
	}, time.Minute)
	if _, err := buildPhases(cr); err == nil {
		t.Error("expected error when spike RPS <= baseRps (would be a dip, not a spike)")
	}
}

func TestBuildPhases_WarmupExceedsSLARejected(t *testing.T) {
	cr := crWithLoad(v1beta1.LoadConfig{
		BaseRPS:        100,
		WarmupDuration: &metav1.Duration{Duration: 2 * time.Minute},
	}, time.Minute)
	if _, err := buildPhases(cr); err == nil {
		t.Error("expected error when warmupDuration >= SLA")
	}
}

func TestBuildPhases_PoissonOK(t *testing.T) {
	cr := crWithLoad(v1beta1.LoadConfig{
		BaseRPS: 100,
		Profile: &v1beta1.LoadProfile{Pattern: "Poisson"},
	}, time.Minute)
	phases, err := buildPhases(cr)
	if err != nil {
		t.Fatalf("buildPhases: %v", err)
	}
	if len(phases) != 1 || phases[0].Pattern != loadgen.PatternPoisson {
		t.Errorf("phases = %+v, want one Poisson phase", phases)
	}
}

func TestBuildPhases_StepOK(t *testing.T) {
	stepRPS := int32(50)
	stepDur := metav1.Duration{Duration: 15 * time.Second}
	cr := crWithLoad(v1beta1.LoadConfig{
		BaseRPS: 100,
		Profile: &v1beta1.LoadProfile{
			Pattern:      "Step",
			StepRPS:      &stepRPS,
			StepDuration: &stepDur,
		},
	}, time.Minute)
	phases, err := buildPhases(cr)
	if err != nil {
		t.Fatalf("buildPhases: %v", err)
	}
	if len(phases) != 1 || phases[0].Pattern != loadgen.PatternStep {
		t.Fatalf("phases = %+v, want one Step phase", phases)
	}
	if phases[0].StepRPS != 50 || phases[0].StepEvery != 15*time.Second {
		t.Errorf("step phase = %+v, want StepRPS=50 / StepEvery=15s", phases[0])
	}
}

func TestBuildPhases_StepRequiresStepRPSAndDuration(t *testing.T) {
	stepRPS := int32(50)
	cr := crWithLoad(v1beta1.LoadConfig{
		BaseRPS: 100,
		Profile: &v1beta1.LoadProfile{
			Pattern: "Step",
			StepRPS: &stepRPS, // missing StepDuration
		},
	}, time.Minute)
	if _, err := buildPhases(cr); err == nil {
		t.Error("expected error when Step missing stepDuration")
	}
}

func TestBuildPhases_UnknownPatternRejected(t *testing.T) {
	cr := crWithLoad(v1beta1.LoadConfig{
		BaseRPS: 100,
		Profile: &v1beta1.LoadProfile{Pattern: "Lognormal"},
	}, time.Minute)
	if _, err := buildPhases(cr); err == nil {
		t.Error("expected error for unknown pattern")
	}
}

func TestBuildPhases_SpikeOverlapRejected(t *testing.T) {
	cr := crWithLoad(v1beta1.LoadConfig{
		BaseRPS: 50,
		Profile: &v1beta1.LoadProfile{
			Pattern: "Spike",
			Spikes: []v1beta1.SpikeWindow{
				{At: metav1.Duration{Duration: 10 * time.Second}, Duration: metav1.Duration{Duration: 5 * time.Second}, RPS: 200},
				// Second spike starts BEFORE the first ends.
				{At: metav1.Duration{Duration: 12 * time.Second}, Duration: metav1.Duration{Duration: 3 * time.Second}, RPS: 300},
			},
		},
	}, time.Minute)
	if _, err := buildPhases(cr); err == nil {
		t.Error("expected error for overlapping spike windows")
	}
}

func TestBuildPhases_SpikePastWindowRejected(t *testing.T) {
	cr := crWithLoad(v1beta1.LoadConfig{
		BaseRPS: 50,
		Profile: &v1beta1.LoadProfile{
			Pattern: "Spike",
			Spikes: []v1beta1.SpikeWindow{
				// Spike ends past the SLA window.
				{At: metav1.Duration{Duration: 55 * time.Second}, Duration: metav1.Duration{Duration: 10 * time.Second}, RPS: 200},
			},
		},
	}, time.Minute)
	if _, err := buildPhases(cr); err == nil {
		t.Error("expected error when spike extends past measurement window")
	}
}

func TestPhasesJSON_RoundTrip(t *testing.T) {
	in := []loadgen.Phase{
		{Name: "warm", Pattern: loadgen.PatternConstant, Duration: time.Second, StartRPS: 10},
		{Name: "measure", Pattern: loadgen.PatternPoisson, Duration: 2 * time.Second, StartRPS: 25, RecordStats: true},
	}
	js, err := phasesJSON(in)
	if err != nil {
		t.Fatalf("phasesJSON: %v", err)
	}
	if js == "" {
		t.Fatal("phasesJSON returned empty string")
	}
}
