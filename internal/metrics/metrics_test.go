package metrics

import "testing"

func TestVerdictFromStatus(t *testing.T) {
	tests := []struct {
		name             string
		sla, trafficInt  string
		want             string
	}{
		{"both pass", "Pass", "Pass", VerdictPass},
		{"sla fail wins", "Fail", "Pass", VerdictFail},
		{"traffic fail wins", "Pass", "Fail", VerdictFail},
		{"both empty unknown", "", "", VerdictUnknown},
		{"mixed pass-empty warn", "Pass", "", VerdictWarn},
		{"unknown values warn", "?", "?", VerdictWarn},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := VerdictFromStatus(tt.sla, tt.trafficInt); got != tt.want {
				t.Errorf("VerdictFromStatus(%q,%q) = %q, want %q",
					tt.sla, tt.trafficInt, got, tt.want)
			}
		})
	}
}
