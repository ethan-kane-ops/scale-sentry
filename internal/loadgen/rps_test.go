package loadgen

import "testing"

func TestDynamicRPS(t *testing.T) {
	tests := []struct {
		name              string
		baseRPS           int
		cpuCores          float64
		concurrencyFactor float64
		want              int
	}{
		{"base only, no cpu scaling", 100, 0, 50, 100},
		{"linear cpu scaling", 100, 4, 50, 300},
		{"fractional cores round half-up", 100, 1.5, 50, 175},
		{"fractional cores round half-down", 100, 0.5, 25, 113}, // round(12.5)=12 (banker's? Go math.Round = away-from-zero → 13)
		{"zero everything yields minimum 1", 0, 0, 0, 1},
		{"negative base clamped to 0", -50, 4, 50, 200},
		{"negative cores clamped to 0", 100, -4, 50, 100},
		{"negative factor clamped to 0", 100, 4, -50, 100},
		{"large values do not overflow at typical bounds", 10000, 16, 250, 14000},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			got := DynamicRPS(tc.baseRPS, tc.cpuCores, tc.concurrencyFactor)
			if got != tc.want {
				t.Fatalf("DynamicRPS(%d, %v, %v) = %d, want %d",
					tc.baseRPS, tc.cpuCores, tc.concurrencyFactor, got, tc.want)
			}
		})
	}
}
