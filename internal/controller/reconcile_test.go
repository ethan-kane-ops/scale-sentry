package controller

import (
	"context"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	appsv1 "k8s.io/api/apps/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
	"github.com/ethan-kane-ops/scale-sentry/internal/metrics"
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

// TestRecordRunMetrics_HPAReactObserved exercises the ScaleUpDuration != nil
// branch of recordRunMetrics: neither integration test in this package sets
// ScaleUpDuration on its observer.Report, so that branch (and the
// HPAReactSeconds observation inside it) was otherwise never hit. Uses a
// namespace/name unique to this test so the label-value child metric it
// creates can't collide with another test's observations on the same
// package-level collector.
func TestRecordRunMetrics_HPAReactObserved(t *testing.T) {
	cr := &v1alpha1.ScaleValidation{
		ObjectMeta: metav1.ObjectMeta{Name: "record-metrics-hpa-react", Namespace: "record-metrics-test"},
	}
	before := testutil.CollectAndCount(metrics.HPAReactSeconds)

	dur := metav1.Duration{Duration: 42 * time.Second}
	recordRunMetrics(cr, observer.Report{
		SLAStatus:        observer.VerdictPass,
		TrafficIntegrity: observer.VerdictPass,
		ScaleUpDuration:  &dur,
	})

	if after := testutil.CollectAndCount(metrics.HPAReactSeconds); after <= before {
		t.Errorf("HPAReactSeconds child count = %d, want > %d (ScaleUpDuration branch not observed)", after, before)
	}
}

func TestAppendRunHistory(t *testing.T) {
	newHistory := func(n int) []v1alpha1.RunSummary {
		h := make([]v1alpha1.RunSummary, n)
		for i := range h {
			h[i] = v1alpha1.RunSummary{Phase: PhaseSucceeded}
		}
		return h
	}

	tests := []struct {
		name         string
		existing     []v1alpha1.RunSummary
		wantLen      int
		wantNewFirst bool
	}{
		{"empty history gets first entry", nil, 1, true},
		{"prepends ahead of existing entries", newHistory(3), 4, true},
		{"truncates at RunHistoryLimit", newHistory(v1alpha1.RunHistoryLimit), v1alpha1.RunHistoryLimit, true},
		{"truncates when already over the limit", newHistory(v1alpha1.RunHistoryLimit + 5), v1alpha1.RunHistoryLimit, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cr := &v1alpha1.ScaleValidation{Status: v1alpha1.ScaleValidationStatus{
				Phase:            PhaseFailed,
				SLAStatus:        "Fail",
				TrafficIntegrity: "Pass",
				FailureRate:      0.12,
				History:          tt.existing,
			}}
			appendRunHistory(cr)

			if len(cr.Status.History) != tt.wantLen {
				t.Fatalf("len(History) = %d, want %d", len(cr.Status.History), tt.wantLen)
			}
			head := cr.Status.History[0]
			if tt.wantNewFirst && (head.Phase != PhaseFailed || head.SLAStatus != "Fail" ||
				head.TrafficIntegrity != "Pass" || head.FailureRate != 0.12) {
				t.Errorf("newest entry not at History[0]: %+v", head)
			}
			if head.FinishedAt.IsZero() {
				t.Error("FinishedAt not set on new entry")
			}
		})
	}
}

func TestTargetReady(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}

	cr := &v1alpha1.ScaleValidation{
		ObjectMeta: metav1.ObjectMeta{Name: "run", Namespace: "demo"},
		Spec: v1alpha1.ScaleValidationSpec{
			TargetRef: v1alpha1.CrossVersionObjectReference{Name: "app"},
		},
	}

	withDeploy := func(replicas int32) *appsv1.Deployment {
		return &appsv1.Deployment{
			ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "demo"},
			Status:     appsv1.DeploymentStatus{ReadyReplicas: replicas},
		}
	}

	tests := []struct {
		name      string
		deploy    *appsv1.Deployment
		wantReady bool
		wantCount int32
	}{
		{"missing target", nil, false, 0},
		{"zero ready replicas", withDeploy(0), false, 0},
		{"one ready replica", withDeploy(1), true, 1},
		{"many ready replicas", withDeploy(5), true, 5},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			b := fake.NewClientBuilder().WithScheme(scheme)
			if tt.deploy != nil {
				b = b.WithObjects(tt.deploy).WithStatusSubresource(tt.deploy)
			}
			r := &ScaleValidationReconciler{Client: b.Build()}
			ready, count, err := r.targetReady(context.Background(), cr)
			if err != nil {
				t.Fatalf("targetReady: %v", err)
			}
			if ready != tt.wantReady || count != tt.wantCount {
				t.Errorf("targetReady = (%v, %d), want (%v, %d)",
					ready, count, tt.wantReady, tt.wantCount)
			}
		})
	}
}
