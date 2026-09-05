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
	targetImage  = "registry.k8s.io/hpa-example"
	targetPort   = int32(80)
	targetCPUReq = "200m"
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

// The disruption scenario never autoscales, so it has no use for
// hpa-example's sqrt loop. What it needs is requests in flight when the
// victim dies, because that is all UngracefulDrain can see, and
// in-flight is rate x service time, not rate. hpa-example's ~50ms of CPU
// per request caps the run at ~10 RPS per pod on a 2-core runner, and
// buying more rate does not help either: measured at 300 RPS against a
// 2ms echo the analyzer still saw exactly 2 drops, because under one
// request was ever in the air and only the client's pooled connections
// could fail.
//
// whoami answers in ~2ms for ~0.1ms of CPU and takes a ?wait= parameter
// that sleeps without burning any, so service time becomes a free,
// directly controlled input. That is the lever this fixture exists to
// provide.
const (
	drainVictimImage = "traefik/whoami:v1.10.3"
	// drainVictimSlowPath holds each request open long enough that a
	// meaningful number are in flight at the kill. Paired with the
	// scenario's rate it sets the expected drop count: at 300 RPS,
	// 200ms of service time means ~60 in flight and ~30 on the victim,
	// far enough above the 1-3 of the old fixture that a single
	// mis-bucketed timestamp can no longer zero the diagnostic.
	drainVictimSlowPath = "/?wait=200ms"
	drainVictimPort     = int32(8080)
	drainVictimCPUReq   = "25m"
	drainVictimCPULim   = "200m"
	drainVictimMemReq   = "32Mi"
	drainVictimMemLim   = "64Mi"
	// drainVictimGrace bounds the gap between SIGTERM and SIGKILL.
	// whoami exits on SIGTERM without draining in-flight requests, which
	// is exactly the behaviour the scenario exists to catch; pinning the
	// grace period states that intent rather than inheriting the 30s
	// default, and guarantees the container is gone well inside the
	// analyzer's 10s drain window.
	//
	// The fixture deliberately has no preStop hook. A preStop sleep is
	// the remediation the UngracefulDrain diagnostic recommends, so
	// adding one here would suppress the signal under test.
	drainVictimGrace = int64(1)
)

// drainVictimDeployment is the target for the disruption scenario: a
// cheap echo server with an explicitly ungraceful shutdown profile. It is
// separate from targetDeployment because the two fixtures want opposite
// things — targetDeployment burns CPU so the HPA reacts, this one burns
// as little as possible so the run can afford rate.
func drainVictimDeployment(ns string, labels map[string]string, replicas int32) *appsv1.Deployment {
	grace := drainVictimGrace
	return &appsv1.Deployment{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: ns, Labels: labels},
		Spec: appsv1.DeploymentSpec{
			Replicas: &replicas,
			Selector: &metav1.LabelSelector{MatchLabels: labels},
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					TerminationGracePeriodSeconds: &grace,
					Containers: []corev1.Container{{
						Name:            "whoami",
						Image:           drainVictimImage,
						ImagePullPolicy: corev1.PullIfNotPresent,
						Args:            []string{fmt.Sprintf("--port=%d", drainVictimPort)},
						Ports:           []corev1.ContainerPort{{Name: "http", ContainerPort: drainVictimPort}},
						Resources: corev1.ResourceRequirements{
							Requests: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse(drainVictimCPUReq),
								corev1.ResourceMemory: resource.MustParse(drainVictimMemReq),
							},
							Limits: corev1.ResourceList{
								corev1.ResourceCPU:    resource.MustParse(drainVictimCPULim),
								corev1.ResourceMemory: resource.MustParse(drainVictimMemLim),
							},
						},
						ReadinessProbe: &corev1.Probe{
							ProbeHandler: corev1.ProbeHandler{
								HTTPGet: &corev1.HTTPGetAction{
									Path: "/", Port: intstr.FromInt32(drainVictimPort),
								},
							},
							PeriodSeconds:    2,
							FailureThreshold: 3,
						},
					}},
				},
			},
		},
	}
}

// drainVictimService fronts drainVictimDeployment on targetPort, so the
// scenario's CR keeps the same spec.target.port as every other fixture.
func drainVictimService(ns string, labels map[string]string) *corev1.Service {
	return &corev1.Service{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: ns, Labels: labels},
		Spec: corev1.ServiceSpec{
			Selector: labels,
			Ports: []corev1.ServicePort{{
				Name: "http", Port: targetPort, TargetPort: intstr.FromInt32(drainVictimPort),
			}},
		},
	}
}
