package observer

import (
	"context"
	"fmt"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ethan-kane-ops/scale-sentry/internal/analyzer/hpa"
	"github.com/ethan-kane-ops/scale-sentry/internal/analyzer/probelag"
)

// target is the resolved set of objects the observer watches for one run.
type target struct {
	// samplePod is the pod used for cgroup scraping.
	samplePod *corev1.Pod
	// newestPod is the most recently created pod — the cold-start subject
	// whose conditions feed the probe-lag analyzer.
	newestPod *corev1.Pod
	// hpa is the HorizontalPodAutoscaler scaling the target, or nil.
	hpa *autoscalingv2.HorizontalPodAutoscaler
}

// resolveTarget looks up the target Deployment, its pods, and its HPA.
func (s *Session) resolveTarget(ctx context.Context) (*target, error) {
	deploy, err := s.clientset.AppsV1().Deployments(s.cfg.Namespace).Get(ctx, s.cfg.TargetName, metav1.GetOptions{})
	if err != nil {
		return nil, fmt.Errorf("get deployment %s: %w", s.cfg.TargetName, err)
	}
	sel, err := metav1.LabelSelectorAsSelector(deploy.Spec.Selector)
	if err != nil {
		return nil, fmt.Errorf("deployment selector: %w", err)
	}
	pods, err := s.clientset.CoreV1().Pods(s.cfg.Namespace).List(ctx, metav1.ListOptions{LabelSelector: sel.String()})
	if err != nil {
		return nil, fmt.Errorf("list target pods: %w", err)
	}
	if len(pods.Items) == 0 {
		return nil, fmt.Errorf("deployment %s has no pods", s.cfg.TargetName)
	}
	return &target{
		samplePod: &pods.Items[0],
		newestPod: newestPod(pods.Items),
		hpa:       s.findHPA(ctx),
	}, nil
}

// newestPod returns the pod with the latest creation timestamp.
func newestPod(pods []corev1.Pod) *corev1.Pod {
	newest := &pods[0]
	for i := range pods {
		if pods[i].CreationTimestamp.After(newest.CreationTimestamp.Time) {
			newest = &pods[i]
		}
	}
	return newest
}

// findHPA returns the HPA whose scaleTargetRef points at the target
// Deployment, or nil if none exists.
func (s *Session) findHPA(ctx context.Context) *autoscalingv2.HorizontalPodAutoscaler {
	list, err := s.clientset.AutoscalingV2().HorizontalPodAutoscalers(s.cfg.Namespace).List(ctx, metav1.ListOptions{})
	if err != nil {
		warn("list HPAs: %v", err)
		return nil
	}
	for i := range list.Items {
		ref := list.Items[i].Spec.ScaleTargetRef
		if ref.Kind == "Deployment" && ref.Name == s.cfg.TargetName {
			return &list.Items[i]
		}
	}
	warn("no HPA targets deployment %s", s.cfg.TargetName)
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

// collectProbeLag re-fetches the cold-start pod and builds its probe-lag
// report from the pod conditions and the readiness probe's periodSeconds.
func (s *Session) collectProbeLag(ctx context.Context, t *target) *probelag.Report {
	if t.newestPod == nil {
		return nil
	}
	pod := t.newestPod
	if fresh, err := s.clientset.CoreV1().Pods(s.cfg.Namespace).Get(ctx, pod.Name, metav1.GetOptions{}); err != nil {
		warn("get pod %s for probelag: %v", pod.Name, err)
	} else {
		pod = fresh
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
