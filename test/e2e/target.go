//go:build e2e

package e2e

import (
	"context"
	"fmt"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	"k8s.io/apimachinery/pkg/api/resource"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"
	"k8s.io/apimachinery/pkg/util/intstr"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	// targetImage is the canonical image used by the Kubernetes HPA
	// walkthrough, a tiny php-apache server that runs a sqrt loop on
	// every request, so CPU usage scales linearly with RPS. Off-the-shelf
	// HTTP servers (nginx / echo / whoami) consume nearly no CPU under
	// load and will not trigger the HPA at all.
	targetImage    = "registry.k8s.io/hpa-example"
	targetPort     = int32(80)
	targetCPUReq   = "200m"
	// hpa-example sits around 40-60 MiB resident; without an explicit
	// request the kubelet will OOMKill pods opportunistically under
	// node memory pressure (which the laptop-sized Kind VM is prone
	// to once 4-5 replicas plus loadgen + observer + controller are
	// all in the air). The limit is generous but bounded.
	targetMemReq   = "96Mi"
	targetMemLim   = "192Mi"
	targetHPAMin   = int32(1)
	targetHPAMax   = int32(5)
	targetHPATgtPc = int32(50)
)

func targetDeployment(ns string, labels map[string]string) *appsv1.Deployment {
	replicas := int32(1)
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: ns, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{Containers: []corev1.Container{{
					Name:            "hpa-example",
					Image:           targetImage,
					ImagePullPolicy: corev1.PullIfNotPresent,
					Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: targetPort}},
					Resources: corev1.ResourceRequirements{
						Requests: corev1.ResourceList{
							corev1.ResourceCPU:    resource.MustParse(targetCPUReq),
							corev1.ResourceMemory: resource.MustParse(targetMemReq),
						},
						Limits: corev1.ResourceList{
							corev1.ResourceMemory: resource.MustParse(targetMemLim),
						},
					},
					ReadinessProbe: &corev1.Probe{
						ProbeHandler: corev1.ProbeHandler{
							TCPSocket: &corev1.TCPSocketAction{Port: intstr.FromInt32(targetPort)},
						},
						PeriodSeconds:    3,
						FailureThreshold: 3,
					},
				}}},
			},
		},
	}
}

func targetService(ns string, labels map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: ns, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name: "http", Port: targetPort, TargetPort: intstr.FromInt32(targetPort),
			}},
		},
	}
}

// targetHPA reacts as fast as the API allows: stabilizationWindowSeconds=0
// so the controller does not damp the scale-up, plus a percent-double
// policy each 15s so the curve to a stable replica count fits inside the
// SLA window. maxReplicas is set high enough that the run lands on a
// stable replica count rather than hitting the cap (which would emit a
// HPAScalingLimited diagnostic).
func targetHPA(ns string) *autoscalingv2.HorizontalPodAutoscaler {
	minR := targetHPAMin
	target := targetHPATgtPc
	stab := int32(0)
	period := int32(15)
	return &autoscalingv2.HorizontalPodAutoscaler{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: ns},
		Spec: autoscalingv2.HorizontalPodAutoscalerSpec{
			ScaleTargetRef: autoscalingv2.CrossVersionObjectReference{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "target",
			},
			MinReplicas: &minR,
			MaxReplicas: targetHPAMax,
			Metrics: []autoscalingv2.MetricSpec{{
				Type: autoscalingv2.ResourceMetricSourceType,
				Resource: &autoscalingv2.ResourceMetricSource{
					Name: corev1.ResourceCPU,
					Target: autoscalingv2.MetricTarget{
						Type:               autoscalingv2.UtilizationMetricType,
						AverageUtilization: &target,
					},
				},
			}},
			Behavior: &autoscalingv2.HorizontalPodAutoscalerBehavior{
				ScaleUp: &autoscalingv2.HPAScalingRules{
					StabilizationWindowSeconds: &stab,
					Policies: []autoscalingv2.HPAScalingPolicy{{
						Type: autoscalingv2.PercentScalingPolicy, Value: 100, PeriodSeconds: period,
					}},
				},
			},
		},
	}
}

// waitForHPAMetrics polls the HPA until metrics-server has scraped at
// least once and the controller has populated status.currentMetrics with
// a non-nil CPU utilisation. Without this, the validation run will fire
// before the HPA can react, and the SLA window starts ticking before the
// scaling subsystem is actually ready.
func waitForHPAMetrics(ctx context.Context, c client.Client, ns, name string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		var h autoscalingv2.HorizontalPodAutoscaler
		if err := c.Get(ctx, types.NamespacedName{Namespace: ns, Name: name}, &h); err != nil {
			return err
		}
		for _, m := range h.Status.CurrentMetrics {
			if m.Resource != nil && m.Resource.Current.AverageUtilization != nil {
				return nil
			}
		}
		time.Sleep(pollInterval)
	}
	return fmt.Errorf("HPA %s/%s has no resource metrics after %s", ns, name, timeout)
}
