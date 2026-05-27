package chaos

import (
	"testing"
	"time"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func pod(name string, phase corev1.PodPhase, ready bool, terminating bool) corev1.Pod {
	p := corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: name},
		Status: corev1.PodStatus{
			Phase: phase,
			Conditions: []corev1.PodCondition{{
				Type:   corev1.PodReady,
				Status: corev1.ConditionFalse,
			}},
		},
	}
	if ready {
		p.Status.Conditions[0].Status = corev1.ConditionTrue
	}
	if terminating {
		now := metav1.Now()
		p.DeletionTimestamp = &now
	}
	return p
}

func TestHealthyPods(t *testing.T) {
	pods := []corev1.Pod{
		pod("ready", corev1.PodRunning, true, false),
		pod("not-ready", corev1.PodRunning, false, false),
		pod("pending", corev1.PodPending, true, false),
		pod("terminating", corev1.PodRunning, true, true),
		pod("ready-2", corev1.PodRunning, true, false),
	}
	healthy := HealthyPods(pods)
	if len(healthy) != 2 {
		t.Fatalf("HealthyPods len = %d, want 2 (%v)", len(healthy), names(healthy))
	}
	if healthy[0].Name != "ready" || healthy[1].Name != "ready-2" {
		t.Errorf("healthy = %v, want [ready ready-2]", names(healthy))
	}
}

func names(pods []corev1.Pod) []string {
	out := make([]string, len(pods))
	for i, p := range pods {
		out[i] = p.Name
	}
	return out
}

func TestPlan(t *testing.T) {
	start := time.Date(2026, 5, 21, 12, 0, 0, 0, time.UTC)
	threeHealthy := []corev1.Pod{
		pod("a", corev1.PodRunning, true, false),
		pod("b", corev1.PodRunning, true, false),
		pod("c", corev1.PodRunning, true, false),
	}

	tests := []struct {
		name        string
		cfg         Config
		pods        []corev1.Pod
		pick        func(int) int
		wantInject  bool
		wantVictim  string
		wantSkip    bool
		wantDisrupt time.Time
	}{
		{
			name:       "disabled, no inject, no skip reason",
			cfg:        Config{InjectPodDeletion: false},
			pods:       threeHealthy,
			wantInject: false,
			wantSkip:   false,
		},
		{
			name:        "enabled, gate met, inject first pod",
			cfg:         Config{InjectPodDeletion: true, MinReplicasForChaos: 2, TriggerDelay: 30 * time.Second},
			pods:        threeHealthy,
			pick:        func(int) int { return 0 },
			wantInject:  true,
			wantVictim:  "a",
			wantDisrupt: start.Add(30 * time.Second),
		},
		{
			name:       "enabled, gate met, picker selects middle pod",
			cfg:        Config{InjectPodDeletion: true, MinReplicasForChaos: 2},
			pods:       threeHealthy,
			pick:       func(int) int { return 1 },
			wantInject: true,
			wantVictim: "b",
		},
		{
			name:       "gate not met, 2 healthy, min 3, skip",
			cfg:        Config{InjectPodDeletion: true, MinReplicasForChaos: 3},
			pods:       threeHealthy[:2],
			pick:       func(int) int { return 0 },
			wantInject: false,
			wantSkip:   true,
		},
		{
			name:       "no healthy pods, skip, no panic",
			cfg:        Config{InjectPodDeletion: true, MinReplicasForChaos: 0},
			pods:       []corev1.Pod{pod("x", corev1.PodPending, false, false)},
			pick:       func(int) int { return 0 },
			wantInject: false,
			wantSkip:   true,
		},
		{
			name:       "out-of-range picker index clamped to 0",
			cfg:        Config{InjectPodDeletion: true, MinReplicasForChaos: 2},
			pods:       threeHealthy,
			pick:       func(int) int { return 99 },
			wantInject: true,
			wantVictim: "a",
		},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			d := Plan(tc.cfg, start, tc.pods, tc.pick)
			if d.Inject != tc.wantInject {
				t.Fatalf("Inject = %v, want %v (skip=%q)", d.Inject, tc.wantInject, d.SkipReason)
			}
			if tc.wantSkip && d.SkipReason == "" {
				t.Error("expected a SkipReason, got empty")
			}
			if !tc.wantSkip && !tc.wantInject && d.SkipReason != "" {
				t.Errorf("expected no SkipReason for disabled config, got %q", d.SkipReason)
			}
			if tc.wantInject {
				if d.Victim == nil {
					t.Fatal("Inject true but Victim is nil")
				}
				if d.Victim.Name != tc.wantVictim {
					t.Errorf("Victim = %q, want %q", d.Victim.Name, tc.wantVictim)
				}
				if !tc.wantDisrupt.IsZero() && !d.DisruptAt.Equal(tc.wantDisrupt) {
					t.Errorf("DisruptAt = %v, want %v", d.DisruptAt, tc.wantDisrupt)
				}
			}
		})
	}
}
