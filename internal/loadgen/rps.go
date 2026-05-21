package loadgen

import "math"

// DynamicRPS computes the target requests-per-second using the formula
//
//	Target RPS = BaseRPS + round(CPUCores * ConcurrencyFactor)
//
// CPUCores is the deployment's allocatable CPU expressed in whole cores
// (millicores / 1000.0). ConcurrencyFactor is the per-core multiplier
// configured on ScaleValidation.spec.load.
//
// Negative inputs are clamped to zero. The result is always >= 1.
func DynamicRPS(baseRPS int, cpuCores, concurrencyFactor float64) int {
	if baseRPS < 0 {
		baseRPS = 0
	}
	if cpuCores < 0 {
		cpuCores = 0
	}
	if concurrencyFactor < 0 {
		concurrencyFactor = 0
	}
	scaled := math.Round(cpuCores * concurrencyFactor)
	total := baseRPS + int(scaled)
	if total < 1 {
		return 1
	}
	return total
}
