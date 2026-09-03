package controller

import (
	"context"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/meta"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	clientgoscheme "k8s.io/client-go/kubernetes/scheme"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/client/fake"

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

func TestTargetURL(t *testing.T) {
	tests := []struct {
		name         string
		scheme, host string
		port         int32
		path, want   string
	}{
		{"root path", "http", "app.demo.svc.cluster.local", 8080, "/", "http://app.demo.svc.cluster.local:8080/"},
		{"empty path normalized", "http", "h", 80, "", "http://h:80/"},
		{"missing leading slash", "http", "h", 80, "ready", "http://h:80/ready"},
		{"https scheme", "https", "h", 443, "/x", "https://h:443/x"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if got := targetURL(tt.scheme, tt.host, tt.port, tt.path); got != tt.want {
				t.Errorf("targetURL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveTargetURL_SpecModes(t *testing.T) {
	// ServiceDefault and CustomPath never touch the API server, so a
	// reconciler with no client is sufficient.
	r := &ScaleValidationReconciler{}
	ctx := context.Background()

	tests := []struct {
		name string
		mod  func(*v1alpha1.ScaleValidation)
		want string
	}{
		{
			name: "service default targets root",
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
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := r.resolveTargetURL(ctx, testCR(tt.mod))
			if err != nil {
				t.Fatalf("resolveTargetURL: %v", err)
			}
			if got != tt.want {
				t.Errorf("resolveTargetURL = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestResolveTargetURL_AutoDiscoverProbe(t *testing.T) {
	scheme := runtime.NewScheme()
	if err := clientgoscheme.AddToScheme(scheme); err != nil {
		t.Fatalf("scheme: %v", err)
	}
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "demo"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:  "web",
					Ports: []corev1.ContainerPort{{Name: "http", ContainerPort: 9000}},
					ReadinessProbe: &corev1.Probe{ProbeHandler: corev1.ProbeHandler{
						HTTPGet: &corev1.HTTPGetAction{
							Path: "/healthz",
							Port: intstr.FromString("http"),
						},
					}},
				}}},
			},
		},
	}
	r := testReconciler(deploy)
	_ = scheme

	cr := testCR(func(cr *v1alpha1.ScaleValidation) {
		cr.Spec.Target.Mode = "AutoDiscoverProbe"
	})
	got, err := r.resolveTargetURL(context.Background(), cr)
	if err != nil {
		t.Fatalf("resolveTargetURL: %v", err)
	}
	// The discovered named port (9000) and path override the spec port.
	if want := "http://app.demo.svc.cluster.local:9000/healthz"; got != want {
		t.Errorf("resolveTargetURL = %q, want %q", got, want)
	}
}

func TestResolveTargetURL_AutoDiscoverProbeMissing(t *testing.T) {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "demo"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{
				Spec: corev1.PodSpec{Containers: []corev1.Container{{Name: "web"}}},
			},
		},
	}
	r := testReconciler(deploy)
	_ = scheme
	cr := testCR(func(cr *v1alpha1.ScaleValidation) {
		cr.Spec.Target.Mode = "AutoDiscoverProbe"
	})
	if _, err := r.resolveTargetURL(context.Background(), cr); err == nil {
		t.Error("expected error when the target has no readiness probe")
	}
}

// TestResolveTargetURL_AutoDiscoverProbeNoContainers covers a target kind
// that carries no pod template containers. AutoDiscoverProbe needs one, so
// the error has to name the workload rather than panic on an empty slice.
func TestResolveTargetURL_AutoDiscoverProbeNoContainers(t *testing.T) {
	deploy := &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "app", Namespace: "demo"},
		Spec: appsv1.DeploymentSpec{
			Template: corev1.PodTemplateSpec{Spec: corev1.PodSpec{}},
		},
	}
	r := testReconciler(deploy)
	cr := testCR(func(cr *v1alpha1.ScaleValidation) {
		cr.Spec.Target.Mode = "AutoDiscoverProbe"
	})
	_, err := r.resolveTargetURL(context.Background(), cr)
	if err == nil {
		t.Fatal("expected an error when the target has no pod template containers")
	}
	if !strings.Contains(err.Error(), "app") {
		t.Errorf("error should name the workload, got %q", err)
	}
}

func TestLoadgenArgs(t *testing.T) {
	args, err := loadgenArgs(testCR(nil), "http://app.demo.svc.cluster.local:8080/", "")
	if err != nil {
		t.Fatalf("loadgenArgs: %v", err)
	}
	want := map[string]string{
		"--url":             "http://app.demo.svc.cluster.local:8080/",
		"--rps":             "150",
		"--duration":        "3m0s",
		"--connection-mode": "KeepAlive",
		"--target-mode":     "ServiceDefault",
		"--network-path":    "ClusterIP",
		"--result-file":     resultFilePath,
	}
	assertFlags(t, args, want)
	if slices.Contains(args, "--tls-insecure-skip-verify") {
		t.Errorf("unexpected --tls-insecure-skip-verify in default args: %v", args)
	}
	if slices.Contains(args, "--tls-ca-bundle") {
		t.Errorf("unexpected --tls-ca-bundle in default args: %v", args)
	}
}

func TestLoadgenArgs_TLSInsecureSkipVerify(t *testing.T) {
	cr := testCR(func(cr *v1alpha1.ScaleValidation) {
		cr.Spec.Target.TLS = &v1alpha1.TLSConfig{InsecureSkipVerify: true}
	})
	args, err := loadgenArgs(cr, "https://app.demo.svc.cluster.local:8443/", "")
	if err != nil {
		t.Fatalf("loadgenArgs: %v", err)
	}
	if !slices.Contains(args, "--tls-insecure-skip-verify") {
		t.Errorf("missing --tls-insecure-skip-verify in args: %v", args)
	}
}

func TestLoadgenArgs_TLSCABundle(t *testing.T) {
	cr := testCR(func(cr *v1alpha1.ScaleValidation) {
		cr.Spec.Target.TLS = &v1alpha1.TLSConfig{
			CABundle: &v1alpha1.CABundleSource{
				ConfigMapRef: v1alpha1.ConfigMapKeyRef{Name: "internal-ca", Key: "ca.crt"},
			},
		}
	})
	args, err := loadgenArgs(cr, "https://app.demo.svc.cluster.local:8443/", "/etc/scale-sentry/tls-ca/ca.crt")
	if err != nil {
		t.Fatalf("loadgenArgs: %v", err)
	}
	assertFlags(t, args, map[string]string{
		"--tls-ca-bundle": "/etc/scale-sentry/tls-ca/ca.crt",
	})
}

func TestLoadgenArgs_Protocol(t *testing.T) {
	cr := testCR(func(cr *v1alpha1.ScaleValidation) {
		cr.Spec.Target.Protocol = "HTTP2"
	})
	args, err := loadgenArgs(cr, "http://app.demo.svc.cluster.local:8080/", "")
	if err != nil {
		t.Fatalf("loadgenArgs: %v", err)
	}
	assertFlags(t, args, map[string]string{"--protocol": "HTTP2"})
}

func TestLoadgenArgs_ProtocolOmittedWhenEmpty(t *testing.T) {
	args, err := loadgenArgs(testCR(nil), "http://app.demo.svc.cluster.local:8080/", "")
	if err != nil {
		t.Fatalf("loadgenArgs: %v", err)
	}
	if slices.Contains(args, "--protocol") {
		t.Errorf("--protocol should be omitted when spec.target.protocol is empty (loadgen default applies): %v", args)
	}
}

// testRESTMapper maps the apps/v1 workload kinds the controller resolves
// spec.targetRef against. The fake client's default mapper is empty, so
// without this any RESTMapping lookup fails with a NoMatch error.
func testRESTMapper() meta.RESTMapper {
	m := meta.NewDefaultRESTMapper([]schema.GroupVersion{{Group: "apps", Version: "v1"}})
	for _, kind := range []string{"Deployment", "StatefulSet", "ReplicaSet"} {
		m.Add(schema.GroupVersionKind{Group: "apps", Version: "v1", Kind: kind}, meta.RESTScopeNamespace)
	}
	return m
}

// testReconciler builds a reconciler whose fake client carries a populated
// RESTMapper, which every spec.targetRef resolution path needs.
func testReconciler(objs ...client.Object) *ScaleValidationReconciler {
	scheme := runtime.NewScheme()
	_ = clientgoscheme.AddToScheme(scheme)
	return &ScaleValidationReconciler{
		Client: fake.NewClientBuilder().
			WithScheme(scheme).
			WithRESTMapper(testRESTMapper()).
			WithObjects(objs...).
			Build(),
	}
}

func TestObserverArgs(t *testing.T) {
	args, err := testReconciler().observerArgs(testCR(nil))
	if err != nil {
		t.Fatalf("observerArgs: %v", err)
	}
	want := map[string]string{
		"--target-name":     "app",
		"--namespace":       "demo",
		"--target-kind":     "Deployment",
		"--target-group":    "apps",
		"--target-version":  "v1",
		"--target-resource": "deployments",
		"--sla":             "3m0s",
		"--result-file":     resultFilePath,
	}
	assertFlags(t, args, want)
}

// TestObserverArgs_StatefulSetTarget is the regression guard for ENG-148:
// spec.targetRef.kind used to be ignored entirely, so a StatefulSet target
// silently produced a Deployment lookup in the observer.
func TestObserverArgs_StatefulSetTarget(t *testing.T) {
	cr := testCR(func(cr *v1alpha1.ScaleValidation) {
		cr.Spec.TargetRef.Kind = "StatefulSet"
	})
	args, err := testReconciler().observerArgs(cr)
	if err != nil {
		t.Fatalf("observerArgs: %v", err)
	}
	assertFlags(t, args, map[string]string{
		"--target-kind":     "StatefulSet",
		"--target-group":    "apps",
		"--target-version":  "v1",
		"--target-resource": "statefulsets",
	})
}

func TestObserverArgs_UnknownKindIsUnresolvable(t *testing.T) {
	cr := testCR(func(cr *v1alpha1.ScaleValidation) {
		cr.Spec.TargetRef.APIVersion = "argoproj.io/v1alpha1"
		cr.Spec.TargetRef.Kind = "Rollout"
	})
	_, err := testReconciler().observerArgs(cr)
	if !errors.Is(err, errTargetUnresolvable) {
		t.Fatalf("err = %v, want errTargetUnresolvable", err)
	}
}

func assertFlags(t *testing.T, args []string, want map[string]string) {
	t.Helper()
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
	r := testReconciler()
	r.LoadgenImage = "registry.test/loadgen:v1"
	r.ObserverImage = "registry.test/observer:v1"
	r.ObserverServiceAccount = "scale-sentry-observer"
	job, err := r.buildLoadgenJob(testCR(nil), "http://app.demo.svc.cluster.local:8080/")
	if err != nil {
		t.Fatalf("buildLoadgenJob: %v", err)
	}

	if job.Name != "run-loadgen" || job.Namespace != "demo" {
		t.Errorf("job identity = %s/%s, want demo/run-loadgen", job.Namespace, job.Name)
	}
	if job.Labels[loadgenForLabel] != "run" {
		t.Errorf("missing loadgen-for label: %v", job.Labels)
	}
	if bl := job.Spec.BackoffLimit; bl == nil || *bl != 0 {
		t.Errorf("BackoffLimit = %v, want 0 (a load run must not retry)", bl)
	}

	pod := job.Spec.Template.Spec
	if pod.ServiceAccountName != "scale-sentry-observer" {
		t.Errorf("ServiceAccountName = %q, want scale-sentry-observer", pod.ServiceAccountName)
	}
	if pod.RestartPolicy != corev1.RestartPolicyNever {
		t.Errorf("RestartPolicy = %s, want Never", pod.RestartPolicy)
	}
	if pod.TerminationGracePeriodSeconds == nil || *pod.TerminationGracePeriodSeconds != jobGracePeriodSeconds {
		t.Errorf("TerminationGracePeriodSeconds = %v, want %d", pod.TerminationGracePeriodSeconds, jobGracePeriodSeconds)
	}

	if len(pod.Containers) != 1 || pod.Containers[0].Image != "registry.test/loadgen:v1" {
		t.Errorf("loadgen container = %+v", pod.Containers)
	}
	if len(pod.InitContainers) != 1 {
		t.Fatalf("want 1 init container (observer sidecar), got %d", len(pod.InitContainers))
	}
	sidecar := pod.InitContainers[0]
	if sidecar.Name != observerContainerName || sidecar.Image != "registry.test/observer:v1" {
		t.Errorf("observer sidecar = %+v", sidecar)
	}
	if sidecar.RestartPolicy == nil || *sidecar.RestartPolicy != corev1.ContainerRestartPolicyAlways {
		t.Errorf("observer RestartPolicy = %v, want Always (native sidecar)", sidecar.RestartPolicy)
	}

	// Both containers must mount the shared run volume.
	for _, c := range [][]corev1.VolumeMount{pod.Containers[0].VolumeMounts, sidecar.VolumeMounts} {
		if len(c) != 1 || c[0].Name != runVolumeName || c[0].MountPath != runVolumePath {
			t.Errorf("volume mount = %+v, want %s at %s", c, runVolumeName, runVolumePath)
		}
	}
	if len(pod.Volumes) != 1 || pod.Volumes[0].EmptyDir == nil {
		t.Errorf("want one emptyDir volume, got %+v", pod.Volumes)
	}
}

// TestBuildLoadgenJob_PSARestrictedHardening asserts the loadgen Job's pod
// and container specs satisfy the Kubernetes PodSecurityAdmission Restricted
// profile, so the chart works on clusters that enforce it namespace-wide.
func TestBuildLoadgenJob_PSARestrictedHardening(t *testing.T) {
	r := testReconciler()
	r.LoadgenImage = "registry.test/loadgen:v1"
	r.ObserverImage = "registry.test/observer:v1"
	r.ObserverServiceAccount = "scale-sentry-observer"
	job, err := r.buildLoadgenJob(testCR(nil), "http://h:80/")
	if err != nil {
		t.Fatalf("buildLoadgenJob: %v", err)
	}
	pod := job.Spec.Template.Spec

	if pod.SecurityContext == nil {
		t.Fatalf("pod SecurityContext must be set for PSA Restricted")
	}
	if pod.SecurityContext.RunAsNonRoot == nil || !*pod.SecurityContext.RunAsNonRoot {
		t.Errorf("pod RunAsNonRoot must be true")
	}
	if sp := pod.SecurityContext.SeccompProfile; sp == nil || sp.Type != corev1.SeccompProfileTypeRuntimeDefault {
		t.Errorf("pod SeccompProfile must be RuntimeDefault, got %+v", sp)
	}

	for _, c := range append(append([]corev1.Container{}, pod.InitContainers...), pod.Containers...) {
		sc := c.SecurityContext
		if sc == nil {
			t.Fatalf("container %s missing SecurityContext", c.Name)
		}
		if sc.AllowPrivilegeEscalation == nil || *sc.AllowPrivilegeEscalation {
			t.Errorf("container %s AllowPrivilegeEscalation must be false", c.Name)
		}
		if sc.RunAsNonRoot == nil || !*sc.RunAsNonRoot {
			t.Errorf("container %s RunAsNonRoot must be true", c.Name)
		}
		if sc.ReadOnlyRootFilesystem == nil || !*sc.ReadOnlyRootFilesystem {
			t.Errorf("container %s ReadOnlyRootFilesystem must be true", c.Name)
		}
		if sc.Capabilities == nil || !slices.Contains(sc.Capabilities.Drop, corev1.Capability("ALL")) {
			t.Errorf("container %s must drop ALL capabilities, got %+v", c.Name, sc.Capabilities)
		}
		if sp := sc.SeccompProfile; sp == nil || sp.Type != corev1.SeccompProfileTypeRuntimeDefault {
			t.Errorf("container %s SeccompProfile must be RuntimeDefault, got %+v", c.Name, sp)
		}
	}
}

func TestBuildLoadgenJob_TLSCABundleMount(t *testing.T) {
	r := testReconciler()
	r.LoadgenImage = "registry.test/loadgen:v1"
	r.ObserverImage = "registry.test/observer:v1"
	r.ObserverServiceAccount = "scale-sentry-observer"
	cr := testCR(func(cr *v1alpha1.ScaleValidation) {
		cr.Spec.Target.TLS = &v1alpha1.TLSConfig{
			CABundle: &v1alpha1.CABundleSource{
				ConfigMapRef: v1alpha1.ConfigMapKeyRef{Name: "internal-ca", Key: "ca.crt"},
			},
		}
	})
	job, err := r.buildLoadgenJob(cr, "https://app.demo.svc.cluster.local:8443/")
	if err != nil {
		t.Fatalf("buildLoadgenJob: %v", err)
	}

	pod := job.Spec.Template.Spec
	var caVol *corev1.Volume
	for i := range pod.Volumes {
		if pod.Volumes[i].Name == tlsCAVolumeName {
			caVol = &pod.Volumes[i]
			break
		}
	}
	if caVol == nil {
		t.Fatalf("missing %s volume on job pod: %+v", tlsCAVolumeName, pod.Volumes)
	}
	if caVol.ConfigMap == nil || caVol.ConfigMap.Name != "internal-ca" {
		t.Errorf("CA volume ConfigMap source = %+v, want name=internal-ca", caVol.ConfigMap)
	}
	if len(caVol.ConfigMap.Items) != 1 || caVol.ConfigMap.Items[0].Key != "ca.crt" {
		t.Errorf("CA volume items = %+v, want one item with key ca.crt", caVol.ConfigMap.Items)
	}

	loadgenMounts := pod.Containers[0].VolumeMounts
	var caMount *corev1.VolumeMount
	for i := range loadgenMounts {
		if loadgenMounts[i].Name == tlsCAVolumeName {
			caMount = &loadgenMounts[i]
			break
		}
	}
	if caMount == nil || caMount.MountPath != tlsCAMountPath || !caMount.ReadOnly {
		t.Errorf("loadgen CA mount = %+v, want readonly at %s", caMount, tlsCAMountPath)
	}

	args := pod.Containers[0].Args
	wantPath := tlsCAMountPath + "/ca.crt"
	if i := slices.Index(args, "--tls-ca-bundle"); i < 0 || i+1 >= len(args) || args[i+1] != wantPath {
		t.Errorf("--tls-ca-bundle flag = %v, want value %q", args, wantPath)
	}

	// The observer sidecar must not receive the CA mount; the bundle is a
	// loadgen-only concern.
	for _, m := range pod.InitContainers[0].VolumeMounts {
		if m.Name == tlsCAVolumeName {
			t.Errorf("observer sidecar should not mount the CA bundle: %+v", m)
		}
	}
}

func TestJobConditionTrue(t *testing.T) {
	jobWith := func(typ batchv1.JobConditionType, s corev1.ConditionStatus) *batchv1.Job {
		return &batchv1.Job{Status: batchv1.JobStatus{
			Conditions: []batchv1.JobCondition{{Type: typ, Status: s}},
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
