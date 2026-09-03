// Package pdb audits a target workload for PodDisruptionBudget coverage.
// A workload with no PDB can lose every replica at once during a node
// drain; a misconfigured PDB can block drains entirely. Both are flagged.
//
// The package is data-only, the controller fetches the workload's pod
// labels and the namespace's PodDisruptionBudgets and passes them in.
package pdb

import (
	"fmt"

	policyv1 "k8s.io/api/policy/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/util/intstr"

	v1beta1 "github.com/ethan-kane-ops/scale-sentry/api/v1beta1"
)

// MatchedPDB is a PodDisruptionBudget whose selector matches the workload.
type MatchedPDB struct {
	Name string
	// BlocksAllDisruption is true when the budget mathematically forbids
	// every voluntary eviction at the current replica count (minAvailable
	// >= replicas, or maxUnavailable resolving to 0).
	BlocksAllDisruption bool
}

// Report is the outcome of [Audit].
type Report struct {
	Replicas     int32
	MatchingPDBs []MatchedPDB
}

// Covered reports whether at least one PDB selects the workload.
func (r Report) Covered() bool { return len(r.MatchingPDBs) > 0 }

// Audit matches the workload's pod labels against the supplied PDBs.
// Returns an error only if a PDB carries an invalid label selector.
func Audit(podLabels map[string]string, replicas int32, pdbs []policyv1.PodDisruptionBudget) (Report, error) {
	r := Report{Replicas: replicas}
	set := labels.Set(podLabels)
	for _, p := range pdbs {
		if p.Spec.Selector == nil {
			continue
		}
		sel, err := metav1.LabelSelectorAsSelector(p.Spec.Selector)
		if err != nil {
			return Report{}, fmt.Errorf("pdb %s selector: %w", p.Name, err)
		}
		if !sel.Matches(set) {
			continue
		}
		r.MatchingPDBs = append(r.MatchingPDBs, MatchedPDB{
			Name:                p.Name,
			BlocksAllDisruption: blocksAll(p.Spec, replicas),
		})
	}
	return r, nil
}

func blocksAll(spec policyv1.PodDisruptionBudgetSpec, replicas int32) bool {
	total := int(replicas)
	if total <= 0 {
		return false
	}
	if spec.MinAvailable != nil {
		// Round up: a percentage minAvailable requires at least the ceiling.
		v, err := intstr.GetScaledValueFromIntOrPercent(spec.MinAvailable, total, true)
		if err == nil && v >= total {
			return true
		}
	}
	if spec.MaxUnavailable != nil {
		// Round down: a percentage maxUnavailable permits at most the floor.
		v, err := intstr.GetScaledValueFromIntOrPercent(spec.MaxUnavailable, total, false)
		if err == nil && v <= 0 {
			return true
		}
	}
	return false
}

// Diagnostics emits a MissingPDB alert when nothing selects the workload,
// plus a PDBBlocksEviction alert for each matching PDB that forbids all
// voluntary eviction.
func (r Report) Diagnostics() []v1beta1.DiagnosticAlert {
	var alerts []v1beta1.DiagnosticAlert
	if !r.Covered() {
		alerts = append(alerts, v1beta1.DiagnosticAlert{
			Type:           "MissingPDB",
			Severity:       "Warning",
			Message:        "no PodDisruptionBudget selects this workload, a node drain or cluster upgrade can evict every replica simultaneously",
			Recommendation: "add a PodDisruptionBudget with minAvailable below the replica count (e.g. minAvailable: 50%)",
		})
	}
	for _, m := range r.MatchingPDBs {
		if m.BlocksAllDisruption {
			alerts = append(alerts, v1beta1.DiagnosticAlert{
				Type:           "PDBBlocksEviction",
				Severity:       "Warning",
				Message:        fmt.Sprintf("PodDisruptionBudget %q permits zero voluntary evictions at %d replica(s), node drains will hang indefinitely", m.Name, r.Replicas),
				Recommendation: "lower minAvailable or raise maxUnavailable so at least one pod can be evicted during a drain",
			})
		}
	}
	return alerts
}
