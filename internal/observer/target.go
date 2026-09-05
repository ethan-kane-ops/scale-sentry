package observer

import (
	"context"
	"fmt"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime/schema"

	"github.com/ethan-kane-ops/scale-sentry/internal/analyzer/dns"
	"github.com/ethan-kane-ops/scale-sentry/internal/analyzer/hpa"
	"github.com/ethan-kane-ops/scale-sentry/internal/analyzer/pdb"
	"github.com/ethan-kane-ops/scale-sentry/internal/analyzer/probelag"
)

// target is the resolved set of objects the observer watches for one run.
type target struct {
	// samplePod is the pod used for cgroup scraping (a pre-existing
	// replica, cgroup analysis tracks the running workload, not the
	// cold-start subject).
	samplePod *corev1.Pod
	// selector is the workload's scale status.selector, stored so the
	// finalization re-lists pods and finds whichever ones the HPA spun up
	// during the run.
	selector string
	// hpa is the HorizontalPodAutoscaler scaling the target, or nil.
	hpa *autoscalingv2.HorizontalPodAutoscaler
}

// applyTargetDefaults fills the workload-identity fields with apps/v1
// Deployment, keeping the observer runnable standalone and preserving the
// behaviour of controllers that predate the flags.
func (c *Config) applyTargetDefaults() {
	if c.TargetKind == "" {
		c.TargetKind = "Deployment"
	}
	if c.TargetResource == "" {
		c.TargetGroup, c.TargetVersion, c.TargetResource = "apps", "v1", "deployments"
	}
	if c.TargetVersion == "" {
		c.TargetVersion = "v1"
	}
}

// targetGVR is the workload as a GroupVersionResource, resolved by the
// controller and passed down as flags.
func (c Config) targetGVR() schema.GroupVersionResource {
	return schema.GroupVersionResource{Group: c.TargetGroup, Version: c.TargetVersion, Resource: c.TargetResource}
}

// resolveTarget looks up the target workload's pods and its HPA. The
// workload's label selector comes from its scale subresource rather than
// from a Deployment's spec.selector, so any scalable kind resolves and the
// selector is exactly the one the HPA itself uses to find pods.
func (s *Session) resolveTarget(ctx context.Context) (*target, error) {
	scaleObj, err := s.dyn.Resource(s.cfg.targetGVR()).Namespace(s.cfg.Namespace).
		Get(ctx, s.cfg.TargetName, metav1.GetOptions{}, "scale")
	if err != nil {
		return nil, fmt.Errorf("get scale of %s %s: %w", s.cfg.TargetKind, s.cfg.TargetName, err)
	}
	selector, _, err := unstructured.NestedString(scaleObj.Object, "status", "selector")
	if err != nil {
		return nil, fmt.Errorf("read scale status.selector of %s %s: %w", s.cfg.TargetKind, s.cfg.TargetName, err)
	}
	if selector == "" {
		return nil, fmt.Errorf("%s %s reports an empty scale status.selector", s.cfg.TargetKind, s.cfg.TargetName)
	}
	pods, err := s.clientset.CoreV1().Pods(s.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: selector})
	if err != nil {
		return nil, fmt.Errorf("list target pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("%s %s has no pods", s.cfg.TargetKind, s.cfg.TargetName)
	}
	return &target{
		samplePod: &pods.Items[0],
		selector:  selector,
		hpa:       s.findHPA(ctx),
	}, nil
}

// pickColdStartPod returns the newest pod whose CreationTimestamp is
// strictly after runStart, i.e. a pod the HPA spun up during the run.
// Returns nil when no such pod exists (no scale-up happened), so the
// probe-lag analyzer is skipped instead of being fed a stale pre-stress
// pod whose conditions describe an unrelated cold start.
func pickColdStartPod(pods []corev1.Pod, runStart time.Time) *corev1.Pod {
	var newest *corev1.Pod
	for i := range pods {
		ct := pods[i].CreationTimestamp.Time
		if !ct.After(runStart) {
			continue
		}
		if newest == nil || ct.After(newest.CreationTimestamp.Time) {
			newest = &pods[i]
		}
	}
	return newest
}

// findHPA returns the HPA whose scaleTargetRef points at the target
// workload, or nil if none exists.
func (s *Session) findHPA(ctx context.Context) *autoscalingv2.HorizontalPodAutoscaler {
	list, err := s.clientset.AutoscalingV2().HorizontalPodAutoscalers(s.cfg.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		warn("list HPAs: %v", err)
		return nil
	}
	for i := range list.Items {
		ref := list.Items[i].Spec.ScaleTargetRef
		if ref.Kind == s.cfg.TargetKind && ref.Name == s.cfg.TargetName {
			return &list.Items[i]
		}
	}
	warn("no HPA targets %s %s", s.cfg.TargetKind, s.cfg.TargetName)
	return nil
}

// snapshotHPA converts a live HPA object into an hpa.Snapshot.
func snapshotHPA(h *autoscalingv2.HorizontalPodAutoscaler, at time.Time) hpa.Snapshot {
	snap := hpa.Snapshot{
		At:              at,
		CurrentReplicas: h.Status.CurrentReplicas,
		DesiredReplicas: h.Status.DesiredReplicas,
		MaxReplicas:     h.Spec.MaxReplicas,
		Conditions:      h.Status.Conditions,
	}
	if h.Spec.MinReplicas != nil {
		snap.MinReplicas = *h.Spec.MinReplicas
	}
	return snap
}

// pollHPA samples the HPA on a ticker until ctx is cancelled. With no HPA
// it simply blocks until ctx is done.
func (s *Session) pollHPA(ctx context.Context, watcher *hpa.Watcher, t *target) {
	if watcher == nil || t.hpa == nil {
		<-ctx.Done()
		return
	}
	name := t.hpa.Name
	ticker := time.NewTicker(s.cfg.PollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			h, err := s.clientset.AutoscalingV2().HorizontalPodAutoscalers(s.cfg.Namespace).Get(ctx, name, metav1.GetOptions{})
			if err != nil {
				warn("get HPA %s: %v", name, err)
				continue
			}
			watcher.Record(snapshotHPA(h, time.Now()))
		}
	}
}

// collectProbeLag re-lists the target's pods and picks the cold-start
// subject, the newest pod the HPA spun up after the run began, then
// builds its probe-lag report from the pod conditions and the readiness
// probe's periodSeconds. Returns nil when no scale-up pod exists so the
// analyzer does not emit a verdict against an unrelated pre-stress pod.
func (s *Session) collectProbeLag(ctx context.Context, t *target, runStart time.Time) *probelag.Report {
	if t == nil || t.selector == "" {
		return nil
	}
	pods, err := s.clientset.CoreV1().Pods(s.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: t.selector})
	if err != nil {
		warn("list pods for probelag: %v", err)
		return nil
	}
	pod := pickColdStartPod(pods.Items, runStart)
	if pod == nil {
		return nil
	}
	r := probelag.FromPodConditions(pod.Status.Conditions, readinessPeriodSeconds(pod))
	return &r
}

// readinessPeriodSeconds returns the first container's readinessProbe
// periodSeconds, or 0 when no readiness probe is configured.
func readinessPeriodSeconds(pod *corev1.Pod) int32 {
	for _, c := range pod.Spec.Containers {
		if c.ReadinessProbe != nil {
			return c.ReadinessProbe.PeriodSeconds
		}
	}
	return 0
}

// collectResilience runs the two analyzers that read cluster configuration
// rather than run-time samples: the target's DNS resolver settings and its
// PodDisruptionBudget coverage. Neither changes during a run, so both are
// evaluated once at finalization, against the replica count the run
// actually settled on rather than the cold-start count it began with (a
// minAvailable that is harmless at five replicas does block every eviction
// at one).
//
// Both are best-effort: a namespace whose RBAC predates the PDB rule
// yields a warning and no PDB verdict, never a failed run.
func (s *Session) collectResilience(ctx context.Context, t *target) resilience {
	var out resilience
	if t == nil {
		return out
	}

	if t.samplePod != nil {
		r, err := dns.Audit(t.samplePod.Spec.DNSConfig)
		if err != nil {
			warn("dns audit: %v", err)
		} else {
			out.dns = &r
		}
	}

	if t.selector == "" || t.samplePod == nil {
		return out
	}
	pdbs, err := s.clientset.PolicyV1().PodDisruptionBudgets(s.cfg.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		warn("list poddisruptionbudgets: %v", err)
		return out
	}
	r, err := pdb.Audit(t.samplePod.Labels, s.currentReplicas(ctx, t), pdbs.Items)
	if err != nil {
		warn("pdb audit: %v", err)
		return out
	}
	out.pdb = &r
	return out
}

// currentReplicas reads the workload's scale subresource for the replica
// count at finalization. Falls back to the HPA's observed count, then to
// the number of pods matching the selector, so a scale read that fails
// mid-teardown degrades the PDB verdict rather than dropping it.
func (s *Session) currentReplicas(ctx context.Context, t *target) int32 {
	scaleObj, err := s.dyn.Resource(s.cfg.targetGVR()).Namespace(s.cfg.Namespace).
		Get(ctx, s.cfg.TargetName, metav1.GetOptions{}, "scale")
	if err == nil {
		if n, found, err := unstructured.NestedInt64(scaleObj.Object, "status", "replicas"); err == nil && found {
			return int32(n)
		}
	}
	if t.hpa != nil && t.hpa.Status.CurrentReplicas > 0 {
		return t.hpa.Status.CurrentReplicas
	}
	pods, err := s.clientset.CoreV1().Pods(s.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: t.selector})
	if err != nil {
		return 0
	}
	return int32(len(pods.Items))
}
