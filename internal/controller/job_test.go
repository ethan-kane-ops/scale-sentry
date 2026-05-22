package controller

import (
	"slices"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
)

func testCR(mod func(*v1alpha1.ScaleValidation)) *v1alpha1.ScaleValidation {
	cr := &v1alpha1.ScaleValidation{
		ObjectMeta: metav1.ObjectMeta{Name: "run", Namespace: "demo"},
		Spec: v1alpha1.ScaleValidationSpec{
			TargetRef: v1alpha1.CrossVersionObjectReference{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "app",
			},
			SLA: metav1.Duration{Duration: 3 * time.Minute},
			Target: v1alpha1.TargetConfig{
				Mode: "ServiceDefault", Port: 8080, NetworkPath: "ClusterIP",
			},
			Load: v1alpha1.LoadConfig{BaseRPS: 150, ConcurrencyFactor: 10},
		},
	}
	if mod != nil {
		mod(cr)
	}
	return cr
}

func TestLoadgenJobName(t *testing.T) {
	got := loadgenJobName(testCR(nil))
	if want := "run-loadgen"; got != want {
		t.Errorf("loadgenJobName = %q, want %q", got, want)
	}
}

func TestResolveTargetURL(t *testing.T) {
	tests := []struct {
		name string
		mod  func(*v1alpha1.ScaleValidation)
		want string
	}{
		{
			name: "service default targets root path",
			mod:  nil,
			want: "http://app.demo.svc.cluster.local:8080/",
		},
		{
			name: "custom path is honored",
			mod: func(cr *v1alpha1.ScaleValidation) {
				cr.Spec.Target.Mode = "CustomPath"
				cr.Spec.Target.CustomPath = "/healthz"
			},
			want: "http://app.demo.svc.cluster.local:8080/healthz",
		},
		{
			name: "custom path without leading slash is normalized",
			mod: func(cr *v1alpha1.ScaleValidation) {
				cr.Spec.Target.Mode = "CustomPath"
				cr.Spec.Target.CustomPath = "ready"
			},
			want: "http://app.demo.svc.cluster.local:8080/ready",
		},
		{
			name: "custom path mode with empty path falls back to root",
			mod: func(cr *v1alpha1.ScaleValidation) {
				cr.Spec.Target.Mode = "CustomPath"
			},
			want: "http://app.demo.svc.cluster.local:8080/",
		},
		{
			name: "auto-discover probe falls back to root in ENG-35",
			mod: func(cr *v1alpha1.ScaleValidation) {
				cr.Spec.Target.Mode = "AutoDiscoverProbe"
			},
			want: "http://app.demo.svc.cluster.local:8080/",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := resolveTargetURL(testCR(tt.mod)); got != tt.want {
				t.Errorf("resolveTargetURL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestLoadgenArgs(t *testing.T) {
	args := loadgenArgs(testCR(nil))

	want := map[string]string{
		"--url":             "http://app.demo.svc.cluster.local:8080/",
		"--rps":             "150",
		"--duration":        "3m0s",
		"--connection-mode": "KeepAlive",
		"--target-mode":     "ServiceDefault",
		"--network-path":    "ClusterIP",
	}
	for flag, wantVal := range want {
		i := slices.Index(args, flag)
		if i < 0 || i+1 >= len(args) {
			t.Errorf("flag %s missing from args %v", flag, args)
			continue
		}
		if args[i+1] != wantVal {
			t.Errorf("flag %s = %q, want %q", flag, args[i+1], wantVal)
		}
	}
}

func TestBuildLoadgenJob(t *testing.T) {
	r := &ScaleValidationReconciler{LoadgenImage: "registry.test/loadgen:v1"}
	job := r.buildLoadgenJob(testCR(nil))

	if job.Name != "run-loadgen" || job.Namespace != "demo" {
		t.Errorf("job identity = %s/%s, want demo/run-loadgen", job.Namespace, job.Name)
	}
	if job.Labels[loadgenForLabel] != "run" {
		t.Errorf("missing loadgen-for label: %v", job.Labels)
	}
	if bl := job.Spec.BackoffLimit; bl == nil || *bl != 0 {
		t.Errorf("BackoffLimit = %v, want 0 (a load run must not retry)", bl)
	}
	c := job.Spec.Template.Spec.Containers
	if len(c) != 1 || c[0].Image != "registry.test/loadgen:v1" {
		t.Errorf("container = %+v, want single loadgen image", c)
	}
	if rp := job.Spec.Template.Spec.RestartPolicy; rp != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %s, want Never", rp)
	}
}

func TestJobConditionTrue(t *testing.T) {
	jobWith := func(t batchv1.JobConditionType, s corev1.ConditionStatus) *batchv1.Job {
		return &batchv1.Job{Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{Type: t, Status: s}},
		}}
	}
	tests := []struct {
		name string
		job  *batchv1.Job
		typ  batchv1.JobConditionType
		want bool
	}{
		{"complete true", jobWith(batchv1.JobComplete, corev1.ConditionTrue), batchv1.JobComplete, true},
		{"failed true", jobWith(batchv1.JobFailed, corev1.ConditionTrue), batchv1.JobFailed, true},
		{"complete false status", jobWith(batchv1.JobComplete, corev1.ConditionFalse), batchv1.JobComplete, false},
		{"condition absent", jobWith(batchv1.JobComplete, corev1.ConditionTrue), batchv1.JobFailed, false},
		{"no conditions", &batchv1.Job{}, batchv1.JobComplete, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := jobConditionTrue(tt.job, tt.typ); got != tt.want {
				t.Errorf("jobConditionTrue = %v, want %v", got, tt.want)
			}
		})
	}
}
