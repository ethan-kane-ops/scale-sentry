package controller

import (
	"testing"

	appsv1 "k8s.io/api/apps/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

func FuzzShadowAnnotations(f *testing.F) {
	f.Add("9090", "90s", "500")
	f.Add("http", "soon", "lots")
	f.Add("-1", "-90s", "-500")
	f.Add("2147483648", "9223372036854775807ns", "2147483648")
	f.Fuzz(func(t *testing.T, port, sla, rps string) {
		deploy := &appsv1.Deployment{ObjectMeta: metav1.ObjectMeta{
			Name:      "target",
			Namespace: "ns",
			Annotations: map[string]string{
				shadowEnableAnnotation: "true",
				shadowPortAnnotation:   port,
				shadowSLAAnnotation:    sla,
				shadowRPSAnnotation:    rps,
			},
		}}
		sv, err := shadowScaleValidation(deploy, "target-shadow")
		if err == nil && sv == nil {
			t.Fatal("nil ScaleValidation with nil error")
		}
	})
}
