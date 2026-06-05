package loadgen

import (
	"context"
	"math"
	"math/rand/v2"
	"time"
)

// scheduleBuffer is the per-phase channel depth handed to workers. Small
// enough that the schedule generator stays roughly in lock-step with the
// dispatching workers (so a stall is visible as a queue, not buried in a
// huge buffer), large enough that brief jitter does not block the emitter.
const scheduleBuffer = 64

// scheduledArrival is one open-loop request slot. The Time field is the
// "ideal" wall-clock instant at which the request was supposed to be
// dispatched. The worker compares completion to this scheduled time so
// queueing delays at the load generator are captured in the latency
// distribution (the coordinated-omission fix). Workers should NOT use
// time.Now() at dispatch as the latency start.
type scheduledArrival struct {
	Time time.Time
}

// runSchedule emits scheduledArrival values for a single Phase. It walks
// the wall-clock interval [phaseStart, phaseStart+phase.Duration) emitting
// one entry per token at the rate dictated by phase.Pattern. The function
// returns when the phase Duration elapses, ctx is cancelled, or the
// receiver stops reading; callers must drain the channel until close.
func runSchedule(ctx context.Context, phaseStart time.Time, phase Phase, out chan<- scheduledArrival) {
	defer close(out)

	end := phaseStart.Add(phase.Duration)
	emit := func(at time.Time) bool {
		if !at.Before(end) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case out <- scheduledArrival{Time: at}:
			return true
		}
	}

	switch phase.Pattern {
	case PatternConstant:
		emitConstant(phaseStart, phase.StartRPS, emit)
	case PatternPoisson:
		emitPoisson(phaseStart, float64(phase.StartRPS), emit)
	case PatternRamp:
		emitRamp(phaseStart, phase.Duration, phase.StartRPS, phase.EndRPS, emit)
	case PatternStep:
		emitStep(phaseStart, phase.Duration, phase.StartRPS, phase.StepRPS, phase.StepEvery, emit)
	}
}

// emitConstant produces tokens at a fixed rate, one every 1/rps seconds.
func emitConstant(phaseStart time.Time, rps int, emit func(time.Time) bool) {
	interval := time.Duration(float64(time.Second) / float64(rps))
	for i := 0; ; i++ {
		at := phaseStart.Add(time.Duration(i) * interval)
		if !emit(at) {
			return
		}
	}
}

// emitPoisson produces tokens with exponential inter-arrival times of
// mean 1/rate, the open-loop model. Burstiness within a window is real,
// matches what user traffic actually looks like, so closed-loop testing
// systematically understates p99 at high rates.
func emitPoisson(phaseStart time.Time, rate float64, emit func(time.Time) bool) {
	at := phaseStart
	for {
		gap := time.Duration(rand.ExpFloat64() / rate * float64(time.Second))
		at = at.Add(gap)
		if !emit(at) {
			return
		}
	}
}

// emitRamp linearly interpolates rate from startRPS at phaseStart to
// endRPS at phaseStart+duration. The token interval is recomputed each
// step so the schedule tracks the rising / falling rate continuously.
func emitRamp(phaseStart time.Time, duration time.Duration, startRPS, endRPS int, emit func(time.Time) bool) {
	at := phaseStart
	totalSeconds := duration.Seconds()
	for {
		elapsed := at.Sub(phaseStart).Seconds()
		if elapsed >= totalSeconds {
			return
		}
		// Current rate from linear interpolation; floor 1 to keep the
		// interval finite even if endRPS is small.
		frac := elapsed / totalSeconds
		rate := float64(startRPS) + frac*float64(endRPS-startRPS)
		if rate < 1 {
			rate = 1
		}
		interval := time.Duration(float64(time.Second) / rate)
		if interval <= 0 {
			interval = time.Microsecond
		}
		if !emit(at) {
			return
		}
		at = at.Add(interval)
	}
}

// emitStep climbs from startRPS by stepRPS every stepEvery, holding each
// plateau as a Constant slice. Used for testing HPA stability under
// monotonic load growth without the smoothness of Ramp.
func emitStep(phaseStart time.Time, duration time.Duration, startRPS, stepRPS int, stepEvery time.Duration, emit func(time.Time) bool) {
	steps := int(math.Ceil(duration.Seconds()/stepEvery.Seconds())) + 1
	for s := 0; s < steps; s++ {
		segStart := phaseStart.Add(time.Duration(s) * stepEvery)
		segRate := startRPS + s*stepRPS
		if segRate < 1 {
			segRate = 1
		}
		interval := time.Duration(float64(time.Second) / float64(segRate))
		segEnd := segStart.Add(stepEvery)
		if d := phaseStart.Add(duration); segEnd.After(d) {
			segEnd = d
		}
		for at := segStart; at.Before(segEnd); at = at.Add(interval) {
			if !emit(at) {
				return
			}
		}
	}
}
