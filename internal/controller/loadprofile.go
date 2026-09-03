package controller

import (
	"encoding/json"
	"fmt"
	"time"

	v1beta1 "github.com/ethan-kane-ops/scale-sentry/api/v1beta1"
	"github.com/ethan-kane-ops/scale-sentry/internal/loadgen"
)

// buildPhases translates the CR's spec.load into the ordered phase list
// the loadgen consumes via `--phases`. Returns nil when the CR carries
// no Profile and no WarmupDuration; callers fall back to the legacy
// single-shot flags in that case so old samples keep working.
//
// The translation rules:
//   - WarmupDuration > 0 prepends a RecordStats=false Constant phase at
//     BaseRPS for the warmup window.
//   - Profile pattern Constant/Poisson/Ramp/Step are emitted as a single
//     measurement phase consuming the remaining SLA budget.
//   - Profile pattern Spike decomposes into a Constant-Spike-Constant-...
//     sequence inside the measurement window.
func buildPhases(cr *v1beta1.ScaleValidation) ([]loadgen.Phase, error) {
	load := cr.Spec.Load
	if load.Profile == nil && (load.WarmupDuration == nil || load.WarmupDuration.Duration == 0) {
		return nil, nil
	}

	base := int(load.BaseRPS)
	sla := cr.Spec.SLA.Duration
	if sla <= 0 {
		return nil, fmt.Errorf("spec.sla must be > 0 for a phased run")
	}

	var phases []loadgen.Phase
	warm := time.Duration(0)
	if load.WarmupDuration != nil {
		warm = load.WarmupDuration.Duration
	}
	if warm < 0 {
		return nil, fmt.Errorf("spec.load.warmupDuration must be >= 0, got %s", warm)
	}
	if warm >= sla {
		return nil, fmt.Errorf("spec.load.warmupDuration (%s) must be < spec.sla (%s) so a measurement window remains",
			warm, sla)
	}
	if warm > 0 {
		phases = append(phases, loadgen.Phase{
			Name:        loadgen.WarmupPhaseName,
			Pattern:     loadgen.PatternConstant,
			Duration:    warm,
			StartRPS:    base,
			RecordStats: false,
		})
	}
	measureDuration := sla - warm

	pattern := loadgen.PatternConstant
	if load.Profile != nil {
		pattern = loadgen.Pattern(load.Profile.Pattern)
	}

	switch pattern {
	case loadgen.PatternConstant, loadgen.PatternPoisson:
		phases = append(phases, loadgen.Phase{
			Name:        loadgen.MeasurePhaseName,
			Pattern:     pattern,
			Duration:    measureDuration,
			StartRPS:    base,
			RecordStats: true,
		})
	case loadgen.PatternRamp:
		if load.Profile.EndRPS == nil {
			return nil, fmt.Errorf("spec.load.profile.endRps required when pattern=Ramp")
		}
		dur := measureDuration
		if load.Profile.RampDuration != nil && load.Profile.RampDuration.Duration > 0 {
			dur = load.Profile.RampDuration.Duration
		}
		phases = append(phases, loadgen.Phase{
			Name:        loadgen.MeasurePhaseName,
			Pattern:     loadgen.PatternRamp,
			Duration:    dur,
			StartRPS:    base,
			EndRPS:      int(*load.Profile.EndRPS),
			RecordStats: true,
		})
	case loadgen.PatternStep:
		if load.Profile.StepRPS == nil || load.Profile.StepDuration == nil {
			return nil, fmt.Errorf("spec.load.profile.stepRps and stepDuration required when pattern=Step")
		}
		phases = append(phases, loadgen.Phase{
			Name:        loadgen.MeasurePhaseName,
			Pattern:     loadgen.PatternStep,
			Duration:    measureDuration,
			StartRPS:    base,
			StepRPS:     int(*load.Profile.StepRPS),
			StepEvery:   load.Profile.StepDuration.Duration,
			RecordStats: true,
		})
	case "Spike":
		// Spike decomposes into Constant base / Constant spike / ... slices
		// stitched together to fill measureDuration. Loadgen's Pattern enum
		// has no Spike value: the controller does the splitting.
		spikePhases, err := decomposeSpike(base, measureDuration, load.Profile.Spikes)
		if err != nil {
			return nil, err
		}
		phases = append(phases, spikePhases...)
	default:
		return nil, fmt.Errorf("unknown spec.load.profile.pattern %q", load.Profile.Pattern)
	}
	return phases, nil
}

// decomposeSpike turns N spike windows into a base/spike/base/... phase
// list filling [0, total). Each phase is RecordStats=true so spikes
// contribute to the verdict. The returned slice is empty only when
// total <= 0.
func decomposeSpike(baseRPS int, total time.Duration, spikes []v1beta1.SpikeWindow) ([]loadgen.Phase, error) {
	if len(spikes) == 0 {
		return nil, fmt.Errorf("spec.load.profile.spikes required when pattern=Spike")
	}
	var phases []loadgen.Phase
	cursor := time.Duration(0)
	for i, sw := range spikes {
		if sw.At.Duration < cursor {
			return nil, fmt.Errorf("spec.load.profile.spikes[%d].at (%s) overlaps the previous spike", i, sw.At.Duration)
		}
		if sw.At.Duration+sw.Duration.Duration > total {
			return nil, fmt.Errorf("spec.load.profile.spikes[%d] extends past the measurement window (sla - warmup = %s)", i, total)
		}
		if sw.RPS <= int32(baseRPS) {
			return nil, fmt.Errorf("spec.load.profile.spikes[%d].rps (%d) must be > baseRps (%d) to be a spike", i, sw.RPS, baseRPS)
		}
		if pre := sw.At.Duration - cursor; pre > 0 {
			phases = append(phases, loadgen.Phase{
				Name:        fmt.Sprintf("%s-base-%d", loadgen.MeasurePhaseName, i),
				Pattern:     loadgen.PatternConstant,
				Duration:    pre,
				StartRPS:    baseRPS,
				RecordStats: true,
			})
		}
		phases = append(phases, loadgen.Phase{
			Name:        fmt.Sprintf("%s-spike-%d", loadgen.MeasurePhaseName, i),
			Pattern:     loadgen.PatternConstant,
			Duration:    sw.Duration.Duration,
			StartRPS:    int(sw.RPS),
			RecordStats: true,
		})
		cursor = sw.At.Duration + sw.Duration.Duration
	}
	if tail := total - cursor; tail > 0 {
		phases = append(phases, loadgen.Phase{
			Name:        loadgen.MeasurePhaseName + "-base-tail",
			Pattern:     loadgen.PatternConstant,
			Duration:    tail,
			StartRPS:    baseRPS,
			RecordStats: true,
		})
	}
	return phases, nil
}

// phasesJSON marshals phases for the loadgen --phases flag. Returned
// string is the literal value passed; controller never logs it.
func phasesJSON(phases []loadgen.Phase) (string, error) {
	b, err := json.Marshal(phases)
	if err != nil {
		return "", fmt.Errorf("marshal phases: %w", err)
	}
	return string(b), nil
}
