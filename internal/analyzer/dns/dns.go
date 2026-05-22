// Package dns statically inspects a pod's DNS configuration for the
// ndots:5 CoreDNS query-amplification antipattern.
//
// Kubernetes defaults ndots to 5. With ndots:5 any name containing fewer
// than 5 dots is first tried against every entry in the search list before
// being resolved absolutely — turning a single external lookup into up to
// ~8-10 CoreDNS queries. At scale this saturates CoreDNS.
package dns

import (
	"fmt"
	"strconv"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
)

// KubernetesDefaultNDots is the ndots value the kubelet applies when a pod
// does not override it via dnsConfig.options.
const KubernetesDefaultNDots = 5

// Report is the outcome of [Audit].
type Report struct {
	// NDots is the effective ndots value — the explicit override when set,
	// otherwise [KubernetesDefaultNDots].
	NDots int
	// Explicit is true when the pod set ndots via dnsConfig.options.
	Explicit bool
}

// Audit resolves the effective ndots from a pod's DNS config. A nil config
// or a config without an ndots option yields the Kubernetes default.
// Returns an error if an ndots option carries a non-integer value.
func Audit(dnsConfig *corev1.PodDNSConfig) (Report, error) {
	if dnsConfig == nil {
		return Report{NDots: KubernetesDefaultNDots, Explicit: false}, nil
	}
	for _, opt := range dnsConfig.Options {
		if opt.Name != "ndots" {
			continue
		}
		if opt.Value == nil {
			return Report{}, fmt.Errorf("dnsConfig option ndots has no value")
		}
		n, err := strconv.Atoi(*opt.Value)
		if err != nil {
			return Report{}, fmt.Errorf("parse ndots value %q: %w", *opt.Value, err)
		}
		return Report{NDots: n, Explicit: true}, nil
	}
	return Report{NDots: KubernetesDefaultNDots, Explicit: false}, nil
}

// Diagnostics emits a DNSNdotsHigh alert when the effective ndots is the
// default 5 (or higher). An explicit ndots below 5 is considered clean.
func (r Report) Diagnostics() []v1alpha1.DiagnosticAlert {
	if r.Explicit && r.NDots < KubernetesDefaultNDots {
		return nil
	}
	source := "the Kubernetes default"
	if r.Explicit {
		source = "an explicit dnsConfig override"
	}
	return []v1alpha1.DiagnosticAlert{{
		Type:     "DNSNdotsHigh",
		Severity: "Warning",
		Message: fmt.Sprintf(
			"pod resolves with ndots:%d (%s) — every non-FQDN lookup walks the search list first, multiplying CoreDNS queries up to %dx",
			r.NDots, source, r.NDots),
		Recommendation: "set dnsConfig.options ndots to 1 or 2, or use fully-qualified names (trailing dot) for external hostnames",
	}}
}
