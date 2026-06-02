package controller

import (
	"testing"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
	"github.com/ethan-kane-ops/scale-sentry/internal/loadgen"
)

func crWithLoad(load v1alpha1.LoadConfig, sla time.Duration) *v1alpha1.ScaleValidation {
	return &v1alpha1.ScaleValidation{
		Spec: v1alpha1.ScaleValidationSpec{
			SLA:  metav1.Duration{Duration: sla},
			Load: load,
		},
	}
}

func TestBuildPhases_NoProfileNoWarmupReturnsNil(t *testing.T) {
	cr := crWithLoad(v1alpha1.LoadConfig{BaseRPS: 100}, time.Minute)
	phases, err := buildPhases(cr)
	if err != nil {
		t.Fatalf("buildPhases: %v", err)
	}
	if phases != nil {
		t.Errorf("phases = %+v, want nil (legacy single-shot path)", phases)
	}
}

func TestBuildPhases_WarmupPrependsRecordStatsFalsePhase(t *testing.T) {
	cr := crWithLoad(v1alpha1.LoadConfig{
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
	cr := crWithLoad(v1alpha1.LoadConfig{
		BaseRPS: 50,
		Profile: &v1alpha1.LoadProfile{Pattern: "Ramp"},
	}, time.Minute)
	if _, err := buildPhases(cr); err == nil {
		t.Error("expected error when Ramp pattern has no endRps")
	}
}

func TestBuildPhases_RampOK(t *testing.T) {
	end := int32(200)
	cr := crWithLoad(v1alpha1.LoadConfig{
		BaseRPS: 50,
		Profile: &v1alpha1.LoadProfile{Pattern: "Ramp", EndRPS: &end},
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
	cr := crWithLoad(v1alpha1.LoadConfig{
		BaseRPS: 50,
		Profile: &v1alpha1.LoadProfile{
			Pattern: "Spike",
			Spikes: []v1alpha1.SpikeWindow{
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
	cr := crWithLoad(v1alpha1.LoadConfig{
		BaseRPS: 100,
		Profile: &v1alpha1.LoadProfile{
			Pattern: "Spike",
			Spikes: []v1alpha1.SpikeWindow{
				{At: metav1.Duration{Duration: 5 * time.Second}, Duration: metav1.Duration{Duration: 5 * time.Second}, RPS: 50},
			},
		},
	}, time.Minute)
	if _, err := buildPhases(cr); err == nil {
		t.Error("expected error when spike RPS <= baseRps (would be a dip, not a spike)")
	}
}

func TestBuildPhases_WarmupExceedsSLARejected(t *testing.T) {
	cr := crWithLoad(v1alpha1.LoadConfig{
		BaseRPS:        100,
		WarmupDuration: &metav1.Duration{Duration: 2 * time.Minute},
	}, time.Minute)
	if _, err := buildPhases(cr); err == nil {
		t.Error("expected error when warmupDuration >= SLA")
	}
}
