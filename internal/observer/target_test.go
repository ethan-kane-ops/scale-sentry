package observer

import (
	"context"
	"errors"
	"strings"
	"testing"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	policyv1 "k8s.io/api/policy/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"k8s.io/apimachinery/pkg/util/intstr"
	dynamicfake "k8s.io/client-go/dynamic/fake"
	kubefake "k8s.io/client-go/kubernetes/fake"
	k8stesting "k8s.io/client-go/testing"
	"k8s.io/utils/ptr"

	v1beta1 "github.com/ethan-kane-ops/scale-sentry/api/v1beta1"
)

func TestApplyTargetDefaults(t *testing.T) {
	tests := []struct {
		name string
		in   Config
		want Config
	}{
		{
			"empty defaults to apps/v1 Deployment",
			Config{},
			Config{TargetKind: "Deployment", TargetGroup: "apps", TargetVersion: "v1", TargetResource: "deployments"},
		},
		{
			"explicit statefulset is left alone",
			Config{TargetKind: "StatefulSet", TargetGroup: "apps", TargetVersion: "v1", TargetResource: "statefulsets"},
			Config{TargetKind: "StatefulSet", TargetGroup: "apps", TargetVersion: "v1", TargetResource: "statefulsets"},
		},
		{
			"core-group resource keeps its empty group",
			Config{TargetKind: "ReplicationController", TargetVersion: "v1", TargetResource: "replicationcontrollers"},
			Config{TargetKind: "ReplicationController", TargetGroup: "", TargetVersion: "v1", TargetResource: "replicationcontrollers"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got := tt.in
			got.applyTargetDefaults()
			if got.TargetKind != tt.want.TargetKind || got.TargetGroup != tt.want.TargetGroup ||
				got.TargetVersion != tt.want.TargetVersion || got.TargetResource != tt.want.TargetResource {
				t.Errorf("applyTargetDefaults = %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestTargetGVR(t *testing.T) {
	cfg := Config{TargetGroup: "apps", TargetVersion: "v1", TargetResource: "statefulsets"}
	want := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	if got := cfg.targetGVR(); got != want {
		t.Errorf("targetGVR = %v, want %v", got, want)
	}
}

// TestFindHPA_MatchesTargetKind is the observer half of the ENG-148
// regression: the kind was hardcoded to "Deployment", so an HPA scaling a
// StatefulSet of the same name was never found and every scale-up
// measurement silently degraded to "no HPA".
func TestFindHPA_MatchesTargetKind(t *testing.T) {
	hpaFor := func(name, kind string) *autoscalingv2.HorizontalPodAutoscaler {
		return &autoscalingv2.HorizontalPodAutoscaler{
			ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "demo"},
			Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
				ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{Kind: kind, Name: "app"},
			},
		}
	}

	tests := []struct {
		name       string
		targetKind string
		wantHPA    string
	}{
		{"statefulset target finds its own HPA", "StatefulSet", "sts-hpa"},
		{"deployment target finds its own HPA", "Deployment", "deploy-hpa"},
		{"kind with no HPA finds nothing", "ReplicaSet", ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			s := &Session{
				cfg:       Config{Namespace: "demo", TargetName: "app", TargetKind: tt.targetKind},
				clientset: kubefake.NewSimpleClientset(hpaFor("deploy-hpa", "Deployment"), hpaFor("sts-hpa", "StatefulSet")),
			}
			got := s.findHPA(context.Background())
			switch {
			case tt.wantHPA == "":
				if got != nil {
					t.Fatalf("findHPA = %s, want nil", got.Name)
				}
			case got == nil:
				t.Fatalf("findHPA = nil, want %s", tt.wantHPA)
			case got.Name != tt.wantHPA:
				t.Errorf("findHPA = %s, want %s", got.Name, tt.wantHPA)
			}
		})
	}
}

// TestResolveTarget_ReadsScaleSubresource pins the least-privilege
// contract: the workload is read through `<resource>/scale`, never as a
// whole object, and its pods come from the scale's status.selector.
func TestResolveTarget_ReadsScaleSubresource(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "statefulsets"}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "StatefulSetList"})

	var sawSubresource string
	dyn.PrependReactor("get", "statefulsets", func(action k8stesting.Action) (bool, runtime.Object, error) {
		sawSubresource = action.GetSubresource()
		return true, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "autoscaling/v1",
			"kind":       "Scale",
			"metadata":   map[string]any{"name": "app", "namespace": "demo"},
			"status":     map[string]any{"replicas": int64(1), "selector": "app=cart"},
		}}, nil
	})

	matching := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "app-0", Namespace: "demo", Labels: map[string]string{"app": "cart"},
	}}
	other := &corev1.Pod{ObjectMeta: metav1.ObjectMeta{
		Name: "unrelated", Namespace: "demo", Labels: map[string]string{"app": "billing"},
	}}

	s := &Session{
		cfg: Config{
			Namespace: "demo", TargetName: "app", TargetKind: "StatefulSet",
			TargetGroup: "apps", TargetVersion: "v1", TargetResource: "statefulsets",
		},
		clientset: kubefake.NewSimpleClientset(matching, other),
		dyn:       dyn,
	}

	tgt, err := s.resolveTarget(context.Background())
	if err != nil {
		t.Fatalf("resolveTarget: %v", err)
	}
	if sawSubresource != "scale" {
		t.Errorf("read subresource %q, want scale (the whole workload must never be fetched)", sawSubresource)
	}
	if tgt.selector != "app=cart" {
		t.Errorf("selector = %q, want app=cart", tgt.selector)
	}
	if tgt.samplePod == nil || tgt.samplePod.Name != "app-0" {
		t.Errorf("samplePod = %v, want the pod behind the scale selector", tgt.samplePod)
	}
}

func TestResolveTarget_EmptySelectorIsAnError(t *testing.T) {
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "DeploymentList"})
	dyn.PrependReactor("get", "deployments", func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "autoscaling/v1",
			"kind":       "Scale",
			"metadata":   map[string]any{"name": "app", "namespace": "demo"},
			"status":     map[string]any{"replicas": int64(1)},
		}}, nil
	})

	s := &Session{
		cfg: Config{
			Namespace: "demo", TargetName: "app", TargetKind: "Deployment",
			TargetGroup: "apps", TargetVersion: "v1", TargetResource: "deployments",
		},
		clientset: kubefake.NewSimpleClientset(),
		dyn:       dyn,
	}
	if _, err := s.resolveTarget(context.Background()); err == nil {
		t.Fatal("expected an error when the scale subresource reports no selector")
	}
}

// scaleReactorSession wires a Session whose scale reads are answered by fn.
func scaleReactorSession(t *testing.T, fn func(k8stesting.Action) (bool, runtime.Object, error), pods ...runtime.Object) *Session {
	t.Helper()
	gvr := schema.GroupVersionResource{Group: "apps", Version: "v1", Resource: "deployments"}
	dyn := dynamicfake.NewSimpleDynamicClientWithCustomListKinds(runtime.NewScheme(),
		map[schema.GroupVersionResource]string{gvr: "DeploymentList"})
	dyn.PrependReactor("get", "deployments", fn)
	return &Session{
		cfg: Config{
			Namespace: "demo", TargetName: "app", TargetKind: "Deployment",
			TargetGroup: "apps", TargetVersion: "v1", TargetResource: "deployments",
		},
		clientset: kubefake.NewSimpleClientset(pods...),
		dyn:       dyn,
	}
}

func TestResolveTarget_ScaleGetErrorNamesTheKind(t *testing.T) {
	s := scaleReactorSession(t, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, nil, apierrors.NewForbidden(
			schema.GroupResource{Group: "apps", Resource: "deployments"}, "app", errors.New("no scale access"))
	})
	_, err := s.resolveTarget(context.Background())
	if err == nil {
		t.Fatal("expected an error when the scale subresource cannot be read")
	}
	if !strings.Contains(err.Error(), "Deployment") || !strings.Contains(err.Error(), "app") {
		t.Errorf("error should name the kind and workload, got %q", err)
	}
}

func TestResolveTarget_NoPodsBehindSelector(t *testing.T) {
	s := scaleReactorSession(t, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{
			"apiVersion": "autoscaling/v1",
			"kind":       "Scale",
			"metadata":   map[string]any{"name": "app", "namespace": "demo"},
			"status":     map[string]any{"replicas": int64(0), "selector": "app=cart"},
		}}, nil
	})
	_, err := s.resolveTarget(context.Background())
	if err == nil {
		t.Fatal("expected an error when no pods sit behind the scale selector")
	}
	if !strings.Contains(err.Error(), "has no pods") {
		t.Errorf("error = %q, want it to say the workload has no pods", err)
	}
}

// scaleReplicasSession wires a Session whose scale reads report replicas,
// with the supplied objects loaded into the typed fake clientset.
func scaleReplicasSession(t *testing.T, replicas int64, objs ...runtime.Object) *Session {
	t.Helper()
	return scaleReactorSession(t, func(k8stesting.Action) (bool, runtime.Object, error) {
		return true, &unstructured.Unstructured{Object: map[string]any{
			"status": map[string]any{"replicas": replicas, "selector": "app=demo"},
		}}, nil
	}, objs...)
}

func victimPod(dnsConfig *corev1.PodDNSConfig) *corev1.Pod {
	return &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{
			Name: "app-1", Namespace: "demo", Labels: map[string]string{"app": "demo"},
		},
		Spec: corev1.PodSpec{DNSConfig: dnsConfig},
	}
}

func budget(name string, minAvailable intstr.IntOrString, selector map[string]string) *policyv1.PodDisruptionBudget {
	return &policyv1.PodDisruptionBudget{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: "demo"},
		Spec: policyv1.PodDisruptionBudgetSpec{
			MinAvailable: &minAvailable,
			Selector:     &metav1.LabelSelector{MatchLabels: selector},
		},
	}
}

func diagTypes(alerts []v1beta1.DiagnosticAlert) map[string]bool {
	out := map[string]bool{}
	for _, a := range alerts {
		out[a.Type] = true
	}
	return out
}

// The DNS and PDB analyzers shipped unwired: both packages existed, were
// unit-tested, and were advertised in the README, but nothing in the
// observer imported them, so DNSNdotsHigh / MissingPDB could never appear
// on a run. These cover the wiring, not the analyzers' own logic.
func TestCollectResilience_UncoveredWorkloadWithDefaultDNS(t *testing.T) {
	pod := victimPod(nil) // no dnsConfig: inherits the ndots:5 default
	s := scaleReplicasSession(t, 3, pod)

	got := s.collectResilience(context.Background(), &target{samplePod: pod, selector: "app=demo"})
	if got.dns == nil {
		t.Fatal("dns report is nil, want the ndots audit to have run")
	}
	if got.dns.NDots != 5 || got.dns.Explicit {
		t.Errorf("dns report = %+v, want the implicit Kubernetes default of 5", *got.dns)
	}
	if got.pdb == nil {
		t.Fatal("pdb report is nil, want the coverage audit to have run")
	}
	if got.pdb.Covered() {
		t.Errorf("pdb report = %+v, want no matching budget", *got.pdb)
	}
	if got.pdb.Replicas != 3 {
		t.Errorf("pdb replicas = %d, want 3 from the scale subresource", got.pdb.Replicas)
	}

	types := diagTypes(append(got.dns.Diagnostics(), got.pdb.Diagnostics()...))
	if !types["DNSNdotsHigh"] || !types["MissingPDB"] {
		t.Errorf("diagnostics = %v, want both DNSNdotsHigh and MissingPDB", types)
	}
}

func TestCollectResilience_CoveredWorkloadWithTunedDNS(t *testing.T) {
	pod := victimPod(&corev1.PodDNSConfig{
		Options: []corev1.PodDNSConfigOption{{Name: "ndots", Value: ptr.To("2")}},
	})
	s := scaleReplicasSession(t, 3, pod, budget("app", intstr.FromInt32(1), map[string]string{"app": "demo"}))

	got := s.collectResilience(context.Background(), &target{samplePod: pod, selector: "app=demo"})
	if got.dns == nil || len(got.dns.Diagnostics()) != 0 {
		t.Errorf("dns diagnostics = %+v, want none for an explicit ndots:2", got.dns)
	}
	if got.pdb == nil || !got.pdb.Covered() {
		t.Errorf("pdb report = %+v, want the matching budget", got.pdb)
	}
	if alerts := got.pdb.Diagnostics(); len(alerts) != 0 {
		t.Errorf("pdb diagnostics = %+v, want none: minAvailable 1 of 3 permits eviction", alerts)
	}
}

// A budget that mathematically forbids every eviction is a separate
// finding from having no budget at all, and it is replica-count
// dependent: minAvailable 3 is fine at 5 replicas and blocks drains
// entirely at 3. The audit therefore has to read the replica count the
// run settled on, not the one it started from.
func TestCollectResilience_BudgetBlocksEvictionAtCurrentReplicas(t *testing.T) {
	pod := victimPod(nil)
	pdbObj := budget("app", intstr.FromInt32(3), map[string]string{"app": "demo"})
	s := scaleReplicasSession(t, 3, pod, pdbObj)

	got := s.collectResilience(context.Background(), &target{samplePod: pod, selector: "app=demo"})
	if got.pdb == nil {
		t.Fatal("pdb report is nil")
	}
	if !diagTypes(got.pdb.Diagnostics())["PDBBlocksEviction"] {
		t.Errorf("diagnostics = %+v, want PDBBlocksEviction at minAvailable=3 of 3 replicas", got.pdb.Diagnostics())
	}
}

// Namespaces whose observer RBAC predates the poddisruptionbudgets grant
// must degrade to "no PDB verdict", never to a failed run.
func TestCollectResilience_PDBListForbiddenStillReportsDNS(t *testing.T) {
	pod := victimPod(nil)
	s := scaleReplicasSession(t, 1, pod)
	s.clientset.(*kubefake.Clientset).PrependReactor("list", "poddisruptionbudgets",
		func(k8stesting.Action) (bool, runtime.Object, error) {
			return true, nil, apierrors.NewForbidden(
				schema.GroupResource{Group: "policy", Resource: "poddisruptionbudgets"}, "", errors.New("no access"))
		})

	got := s.collectResilience(context.Background(), &target{samplePod: pod, selector: "app=demo"})
	if got.pdb != nil {
		t.Errorf("pdb report = %+v, want nil when the list is forbidden", *got.pdb)
	}
	if got.dns == nil {
		t.Error("dns report is nil, a forbidden PDB list must not suppress the DNS audit")
	}
}

func TestCollectResilience_NilTarget(t *testing.T) {
	s := scaleReplicasSession(t, 1)
	got := s.collectResilience(context.Background(), nil)
	if got.dns != nil || got.pdb != nil {
		t.Errorf("resilience = %+v, want both halves nil for a nil target", got)
	}
}
