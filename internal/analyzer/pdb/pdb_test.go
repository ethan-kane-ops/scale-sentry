package pdb

import (
	"testing"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/util/intstr"
)

func makePDB(name string, matchLabels map[string]string, minAvail, maxUnavail *intstr.IntOrString) policyv1.PodDisruptionBudget {
	return policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Spec: policyv1.PodDisruptionBudgetSpec{
			Selector:       &metav1.LabelSelector{MatchLabels: matchLabels},
			MinAvailable:   minAvail,
			MaxUnavailable: maxUnavail,
		},
	}
}

func intOrStr(i int32) *intstr.IntOrString { v := intstr.FromInt32(i); return &v }
func pctOrStr(s string) *intstr.IntOrString { v := intstr.FromString(s); return &v }

func TestAudit_Coverage(t *testing.T) {
	appLabels := map[string]string{"app": "checkout"}

	tests := []struct {
		name        string
		pdbs        []policyv1.PodDisruptionBudget
		replicas    int32
		wantCovered bool
		wantMatches int
	}{
		{
			name:        "no PDBs, uncovered",
			pdbs:        nil,
			replicas:    3,
			wantCovered: false,
		},
		{
			name:        "matching selector, covered",
			pdbs:        []policyv1.PodDisruptionBudget{makePDB("a", map[string]string{"app": "checkout"}, intOrStr(1), nil)},
			replicas:    3,
			wantCovered: true,
			wantMatches: 1,
		},
		{
			name:        "non-matching selector, uncovered",
			pdbs:        []policyv1.PodDisruptionBudget{makePDB("a", map[string]string{"app": "other"}, intOrStr(1), nil)},
			replicas:    3,
			wantCovered: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Audit(appLabels, tc.replicas, tc.pdbs)
			if err != nil {
				t.Fatalf("Audit: %v", err)
			}
			if r.Covered() != tc.wantCovered {
				t.Errorf("Covered() = %v, want %v", r.Covered(), tc.wantCovered)
			}
			if len(r.MatchingPDBs) != tc.wantMatches {
				t.Errorf("MatchingPDBs len = %d, want %d", len(r.MatchingPDBs), tc.wantMatches)
			}
		})
	}
}

func TestAudit_BlocksAllDisruption(t *testing.T) {
	appLabels := map[string]string{"app": "checkout"}

	tests := []struct {
		name       string
		pdb        policyv1.PodDisruptionBudget
		replicas   int32
		wantBlocks bool
	}{
		{
			name:       "minAvailable below replicas, does not block",
			pdb:        makePDB("a", appLabels, intOrStr(2), nil),
			replicas:   3,
			wantBlocks: false,
		},
		{
			name:       "minAvailable equals replicas, blocks",
			pdb:        makePDB("a", appLabels, intOrStr(3), nil),
			replicas:   3,
			wantBlocks: true,
		},
		{
			name:       "minAvailable above replicas, blocks",
			pdb:        makePDB("a", appLabels, intOrStr(5), nil),
			replicas:   3,
			wantBlocks: true,
		},
		{
			name:       "minAvailable 100%, blocks",
			pdb:        makePDB("a", appLabels, pctOrStr("100%"), nil),
			replicas:   4,
			wantBlocks: true,
		},
		{
			name:       "maxUnavailable 0, blocks",
			pdb:        makePDB("a", appLabels, nil, intOrStr(0)),
			replicas:   3,
			wantBlocks: true,
		},
		{
			name:       "maxUnavailable 1, does not block",
			pdb:        makePDB("a", appLabels, nil, intOrStr(1)),
			replicas:   3,
			wantBlocks: false,
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			r, err := Audit(appLabels, tc.replicas, []policyv1.PodDisruptionBudget{tc.pdb})
			if err != nil {
				t.Fatalf("Audit: %v", err)
			}
			if len(r.MatchingPDBs) != 1 {
				t.Fatalf("expected 1 match, got %d", len(r.MatchingPDBs))
			}
			if r.MatchingPDBs[0].BlocksAllDisruption != tc.wantBlocks {
				t.Errorf("BlocksAllDisruption = %v, want %v", r.MatchingPDBs[0].BlocksAllDisruption, tc.wantBlocks)
			}
		})
	}
}

func TestDiagnostics(t *testing.T) {
	appLabels := map[string]string{"app": "checkout"}

	t.Run("uncovered emits MissingPDB", func(t *testing.T) {
		r, _ := Audit(appLabels, 3, nil)
		alerts := r.Diagnostics()
		if len(alerts) != 1 || alerts[0].Type != "MissingPDB" {
			t.Fatalf("alerts = %+v, want one MissingPDB", alerts)
		}
		if alerts[0].Severity != "Warning" {
			t.Errorf("severity = %q, want Warning", alerts[0].Severity)
		}
	})

	t.Run("healthy PDB emits nothing", func(t *testing.T) {
		r, _ := Audit(appLabels, 3, []policyv1.PodDisruptionBudget{makePDB("a", appLabels, intOrStr(1), nil)})
		if alerts := r.Diagnostics(); len(alerts) != 0 {
			t.Errorf("alerts = %+v, want none", alerts)
		}
	})

	t.Run("blocking PDB emits PDBBlocksEviction", func(t *testing.T) {
		r, _ := Audit(appLabels, 3, []policyv1.PodDisruptionBudget{makePDB("a", appLabels, intOrStr(3), nil)})
		alerts := r.Diagnostics()
		if len(alerts) != 1 || alerts[0].Type != "PDBBlocksEviction" {
			t.Fatalf("alerts = %+v, want one PDBBlocksEviction", alerts)
		}
	})
}
