package controller

import (
	"testing"

	corev1 "k8s.io/api/core/v1"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
	"github.com/ethan-kane-ops/scale-sentry/internal/observer"
)

func TestObserverTerminated(t *testing.T) {
	withState := func(s corev1.ContainerState) *corev1.Pod {
		return &corev1.Pod{Status: corev1.PodStatus{
			InitContainerStatuses: []corev1.ContainerStatus{{Name: observerContainerName, State: s}},
		}}
	}
	tests := []struct {
		name string
		pod  *corev1.Pod
		want bool
	}{
		{"terminated", withState(corev1.ContainerState{Terminated: &corev1.ContainerStateTerminated{}}), true},
		{"running", withState(corev1.ContainerState{Running: &corev1.ContainerStateRunning{}}), false},
		{"no observer status", &corev1.Pod{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := observerTerminated(tt.pod); got != tt.want {
				t.Errorf("observerTerminated = %v, want %v", got, tt.want)
			}
		})
	}
}

func TestApplyReport(t *testing.T) {
	cr := &v1alpha1.ScaleValidation{}
	applyReport(cr, observer.Report{
		Diagnostics: []v1alpha1.DiagnosticAlert{
			{Type: "CPUThrottling", Severity: "Warning", Message: "throttled"},
		},
		SLAStatus:        observer.VerdictPass,
		TrafficIntegrity: observer.VerdictFail,
		TotalRequests:    1000,
		FailedRequests:   40,
		FailureRate:      0.04,
	})

	if len(cr.Status.Diagnostics) != 1 || cr.Status.Diagnostics[0].Type != "CPUThrottling" {
		t.Errorf("diagnostics not copied: %+v", cr.Status.Diagnostics)
	}
	if cr.Status.SLAStatus != "Pass" || cr.Status.TrafficIntegrity != "Fail" {
		t.Errorf("verdicts = %s/%s, want Pass/Fail", cr.Status.SLAStatus, cr.Status.TrafficIntegrity)
	}
	if cr.Status.TotalRequests != 1000 || cr.Status.FailedRequests != 40 || cr.Status.FailureRate != 0.04 {
		t.Errorf("metrics not copied: %+v", cr.Status)
	}
}
