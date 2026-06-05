package loadgen

import (
	"errors"
	"fmt"
	"time"
)

// Pattern selects the arrival shape for a [Phase]. Constant emits requests
// at a fixed rate, Poisson emits with exponential inter-arrival times
// (open-loop, more realistic for user traffic), Ramp linearly interpolates
// between StartRPS and EndRPS over Duration, Step climbs in fixed
// increments every StepEvery, and Spike is a Constant of higher rate
// stitched into a measurement window by the controller.
type Pattern string

const (
	PatternConstant Pattern = "Constant"
	PatternPoisson  Pattern = "Poisson"
	PatternRamp     Pattern = "Ramp"
	PatternStep     Pattern = "Step"
)

// Phase is a single arrival-rate segment of a Run. Phases execute in
// sequence; each carries its own pattern + rate parameters. Warmup phases
// set RecordStats false so the requests are sent (for TCP/TLS slow-start,
// JIT, page-cache warming) but their latencies and counts are excluded
// from the histogram + verdict.
type Phase struct {
	// Name is a human label written into Result.Phases and Labels.
	// "Warmup" is reserved: phases named Warmup auto-default to
	// RecordStats=false.
	Name string

	// Pattern selects the arrival shape. Required.
	Pattern Pattern

	// Duration is the wall-clock length of this phase. Required.
	Duration time.Duration

	// StartRPS is the rate for Constant and Poisson, and the initial rate
	// for Ramp and Step. Required, must be > 0.
	StartRPS int

	// EndRPS is the terminal rate for Ramp. Ignored for other patterns.
	EndRPS int

	// StepRPS is the rate increment per StepEvery interval for Step.
	StepRPS int

	// StepEvery is the wall-clock interval between Step climbs.
	StepEvery time.Duration

	// RecordStats false sends requests but discards their measurements,
	// keeping the cold-start window out of the histogram + verdict.
	RecordStats bool
}

// WarmupPhaseName is the conventional Name for the cold-start warmup
// phase; sample CRs and controller translation both use this string.
const WarmupPhaseName = "Warmup"

// MeasurePhaseName is the conventional Name for the steady-state
// measurement phase that drives SLA verdicts.
const MeasurePhaseName = "Measure"

// Validate returns an error if p is missing required fields or holds
// pattern-specific values the schedule cannot evaluate.
func (p Phase) Validate() error {
	if p.Name == "" {
		return errors.New("phase name is required")
	}
	if p.Duration <= 0 {
		return fmt.Errorf("phase %q duration must be > 0, got %s", p.Name, p.Duration)
	}
	if p.StartRPS <= 0 {
		return fmt.Errorf("phase %q startRPS must be > 0, got %d", p.Name, p.StartRPS)
	}
	switch p.Pattern {
	case PatternConstant, PatternPoisson:
		// no extra fields required
	case PatternRamp:
		if p.EndRPS <= 0 {
			return fmt.Errorf("phase %q pattern=Ramp requires endRPS > 0, got %d", p.Name, p.EndRPS)
		}
	case PatternStep:
		if p.StepRPS <= 0 {
			return fmt.Errorf("phase %q pattern=Step requires stepRPS > 0, got %d", p.Name, p.StepRPS)
		}
		if p.StepEvery <= 0 {
			return fmt.Errorf("phase %q pattern=Step requires stepEvery > 0, got %s", p.Name, p.StepEvery)
		}
	case "":
		return fmt.Errorf("phase %q pattern is required", p.Name)
	default:
		return fmt.Errorf("phase %q unknown pattern %q", p.Name, p.Pattern)
	}
	return nil
}

// peakRPS returns the maximum rate reached by p across its Duration. Used
// to size the worker pool: workers must keep up with the highest-rate
// phase, so the pool is dimensioned for the run's peak.
func (p Phase) peakRPS() int {
	switch p.Pattern {
	case PatternRamp:
		if p.EndRPS > p.StartRPS {
			return p.EndRPS
		}
		return p.StartRPS
	case PatternStep:
		// Climb caps at floor(Duration/StepEvery) climbs above StartRPS.
		climbs := int(p.Duration / p.StepEvery)
		return p.StartRPS + climbs*p.StepRPS
	default: // Constant, Poisson
		return p.StartRPS
	}
}
