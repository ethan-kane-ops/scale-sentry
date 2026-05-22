package controller

import (
	"testing"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func testDeployment(annotations map[string]string) *appsv1.Deployment {
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{
			Name:        "web",
			Namespace:   "demo",
			Annotations: annotations,
		},
	}
}

func TestShadowScaleValidation_Defaults(t *testing.T) {
	sv, err := shadowScaleValidation(testDeployment(nil), "web-shadow")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sv.Name != "web-shadow" || sv.Namespace != "demo" {
		t.Errorf("identity = %s/%s, want demo/web-shadow", sv.Namespace, sv.Name)
	}
	if ref := sv.Spec.TargetRef; ref.Name != "web" || ref.Kind != "Deployment" {
		t.Errorf("targetRef = %+v, want Deployment/web", ref)
	}
	if sv.Spec.Target.Port != shadowDefaultPort {
		t.Errorf("port = %d, want default %d", sv.Spec.Target.Port, shadowDefaultPort)
	}
	if sv.Spec.SLA.Duration != shadowDefaultSLA {
		t.Errorf("sla = %s, want default %s", sv.Spec.SLA.Duration, shadowDefaultSLA)
	}
	if sv.Spec.Load.BaseRPS != shadowDefaultBaseRPS {
		t.Errorf("baseRps = %d, want default %d", sv.Spec.Load.BaseRPS, shadowDefaultBaseRPS)
	}
}

func TestShadowScaleValidation_AnnotationOverrides(t *testing.T) {
	sv, err := shadowScaleValidation(testDeployment(map[string]string{
		shadowPortAnnotation: "9090",
		shadowSLAAnnotation:  "90s",
		shadowRPSAnnotation:  "500",
	}), "web-shadow")
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}

	if sv.Spec.Target.Port != 9090 {
		t.Errorf("port = %d, want 9090", sv.Spec.Target.Port)
	}
	if sv.Spec.SLA.Duration != 90*time.Second {
		t.Errorf("sla = %s, want 1m30s", sv.Spec.SLA.Duration)
	}
	if sv.Spec.Load.BaseRPS != 500 {
		t.Errorf("baseRps = %d, want 500", sv.Spec.Load.BaseRPS)
	}
}

func TestShadowScaleValidation_InvalidAnnotations(t *testing.T) {
	tests := []struct {
		name string
		anns map[string]string
	}{
		{"non-numeric port", map[string]string{shadowPortAnnotation: "http"}},
		{"unparseable sla", map[string]string{shadowSLAAnnotation: "soon"}},
		{"non-numeric rps", map[string]string{shadowRPSAnnotation: "lots"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := shadowScaleValidation(testDeployment(tt.anns), "web-shadow"); err == nil {
				t.Errorf("expected error for %s, got nil", tt.name)
			}
		})
	}
}
