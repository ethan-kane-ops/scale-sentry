// Package chaos plans pod-disruption injection for a ScaleValidation run.
// It decides whether, when, and which pod to terminate, the actual
// deletion is the controller's job (the controller holds the K8s client).
//
// The disruption is deliberately conservative: it refuses to inject unless
// at least spec.disruption.minReplicasForChaos healthy replicas exist, so a
// validation run cannot itself cause a full outage.
package chaos

import (
	"fmt"
	"math/rand/v2"
	"time"

	corev1 "k8s.io/api/core/v1"
)

// Config mirrors api/v1alpha1.DisruptionConfig with the trigger delay
// already resolved to a time.Duration.
type Config struct {
	InjectPodDeletion   bool
	MinReplicasForChaos int32
	TriggerDelay        time.Duration
}

// Decision is the planned disruption.
type Decision struct {
	// Inject is true when a victim was selected and should be deleted.
	Inject bool
	// SkipReason is set when InjectPodDeletion was true but gating
	// prevented the disruption (e.g. not enough healthy replicas).
	// Empty when Inject is true or disruption was simply disabled.
	SkipReason string
	// DisruptAt is the wall-clock time the controller should delete Victim.
	DisruptAt time.Time
	// Victim is the pod selected for termination. Nil when Inject is false.
	Victim *corev1.Pod
	// HealthyCount is the number of healthy pods observed at plan time.
	HealthyCount int
}

// HealthyPods filters pods to those that are Running, Ready, and not
// already being terminated. Only healthy pods are eligible victims, // killing an already-unhealthy pod would not test graceful shutdown.
func HealthyPods(pods []corev1.Pod) []corev1.Pod {
	healthy := make([]corev1.Pod, 0, len(pods))
	for _, p := range pods {
		if p.DeletionTimestamp != nil {
			continue
		}
		if p.Status.Phase != corev1.PodRunning {
			continue
		}
		if !podReady(p) {
			continue
		}
		healthy = append(healthy, p)
	}
	return healthy
}

func podReady(p corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}

// Plan evaluates the disruption config against the current pod set.
// loadStart is when the load phase began; DisruptAt is loadStart+TriggerDelay.
//
// pick selects the victim index in [0, n); pass a deterministic function
// in tests. A nil pick uses math/rand/v2.IntN.
func Plan(cfg Config, loadStart time.Time, pods []corev1.Pod, pick func(n int) int) Decision {
	if !cfg.InjectPodDeletion {
		return Decision{Inject: false}
	}

	healthy := HealthyPods(pods)
	d := Decision{HealthyCount: len(healthy)}

	if len(healthy) == 0 || len(healthy) < int(cfg.MinReplicasForChaos) {
		d.SkipReason = fmt.Sprintf(
			"only %d healthy replica(s); minReplicasForChaos is %d, skipping disruption to avoid total unavailability",
			len(healthy), cfg.MinReplicasForChaos)
		return d
	}

	if pick == nil {
		pick = rand.IntN
	}
	idx := pick(len(healthy))
	if idx < 0 || idx >= len(healthy) {
		idx = 0
	}
	victim := healthy[idx]

	d.Inject = true
	d.Victim = &victim
	d.DisruptAt = loadStart.Add(cfg.TriggerDelay)
	return d
}
