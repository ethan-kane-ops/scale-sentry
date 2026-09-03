package controller

import (
	"errors"
	"testing"
	"time"

	"github.com/prometheus/client_golang/prometheus/testutil"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime/schema"

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

func TestTargetGVK(t *testing.T) {
	tests := []struct {
		name       string
		apiVersion string
		kind       string
		want       schema.GroupVersionKind
		wantErr    bool
	}{
		{"apps deployment", "apps/v1", "Deployment", schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, false},
		{"apps statefulset", "apps/v1", "StatefulSet", schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "StatefulSet"}, false},
		{"custom group", "argoproj.io/v1alpha1", "Rollout", schema.GroupVersionKind{Group: "argoproj.io", Version: "v1alpha1", Kind: "Rollout"}, false},
		{"empty apiVersion defaults to apps/v1", "", "Deployment", schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: "Deployment"}, false},
		{"empty kind", "apps/v1", "", schema.GroupVersionKind{}, true},
		{"malformed apiVersion", "a/b/c", "Deployment", schema.GroupVersionKind{}, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			cr := &v1alpha1.ScaleValidation{
				Spec: v1alpha1.ScaleValidationSpec{
					TargetRef: v1alpha1.CrossVersionObjectReference{
						APIVersion: tt.apiVersion, Kind: tt.kind, Name: "app",
					},
				},
			}
			got, err := targetGVK(cr)
			if tt.wantErr {
				if !errors.Is(err, errTargetUnresolvable) {
					t.Fatalf("err = %v, want errTargetUnresolvable", err)
				}
				return
			}
			if err != nil {
				t.Fatalf("targetGVK: %v", err)
			}
			if got != tt.want {
				t.Errorf("targetGVK = %v, want %v", got, tt.want)
			}
		})
	}
}

// TestClassifyTargetError pins the distinction ENG-148 turns on: a kind
// the cluster does not serve is a spec error the run must fail on, while a
// NotFound is the normal "workload not applied yet" case the readiness
// gate keeps waiting through.
func TestClassifyTargetError(t *testing.T) {
	cr := &v1alpha1.ScaleValidation{
		Spec: v1alpha1.ScaleValidationSpec{
			TargetRef: v1alpha1.CrossVersionObjectReference{
				APIVersion: "argoproj.io/v1alpha1", Kind: "Rollout", Name: "app",
			},
		},
	}
	gk := schema.GroupKind{Group: "argoproj.io", Kind: "Rollout"}

	if err := classifyTargetError(cr, &meta.NoKindMatchError{GroupKind: gk}); !errors.Is(err, errTargetUnresolvable) {
		t.Errorf("no-match error = %v, want errTargetUnresolvable", err)
	}

	forbidden := apierrors.NewForbidden(schema.GroupResource{Group: "argoproj.io", Resource: "rollouts"}, "app", errors.New("nope"))
	if err := classifyTargetError(cr, forbidden); !errors.Is(err, errTargetUnresolvable) {
		t.Errorf("forbidden error = %v, want errTargetUnresolvable", err)
	}

	notFound := apierrors.NewNotFound(schema.GroupResource{Group: "apps", Resource: "deployments"}, "app")
	err := classifyTargetError(cr, notFound)
	if errors.Is(err, errTargetUnresolvable) {
		t.Errorf("NotFound must stay retryable, got errTargetUnresolvable: %v", err)
	}
	if !apierrors.IsNotFound(err) {
		t.Errorf("NotFound must pass through unchanged, got %v", err)
	}
}

func TestPodReady(t *testing.T) {
	tests := []struct {
		name string
		pod  corev1.Pod
		want bool
	}{
		{"ready", corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionTrue}}}}, true},
		{"running but not ready", corev1.Pod{Status: corev1.PodStatus{Conditions: []corev1.PodCondition{
			{Type: corev1.PodReady, Status: corev1.ConditionFalse}}}}, false},
		{"no conditions", corev1.Pod{}, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := podReady(&tt.pod); got != tt.want {
				t.Errorf("podReady = %v, want %v", got, tt.want)
			}
		})
	}
}
