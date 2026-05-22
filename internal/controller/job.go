package controller

import (
	"fmt"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
)

// loadgenForLabel marks a Job (and its pods) as belonging to a ScaleValidation.
const loadgenForLabel = "validation.scale-sentry.ek.co/loadgen-for"

// defaultLoadgenConnectionMode is the connection mode passed to every loadgen
// run. ENG-36 may surface this on the CRD spec.
const defaultLoadgenConnectionMode = "KeepAlive"

// loadgenJobName is the deterministic name of the loadgen Job for cr, so
// reconciliation can find an existing run instead of spawning duplicates.
func loadgenJobName(cr *v1alpha1.ScaleValidation) string {
	return cr.Name + "-loadgen"
}

// buildLoadgenJob constructs the loadgen Job for cr. BackoffLimit is 0 — a
// load run is a measurement, not a retryable unit of work.
func (r *ScaleValidationReconciler) buildLoadgenJob(cr *v1alpha1.ScaleValidation) *batchv1.Job {
	backoffLimit := int32(0)
	labels := map[string]string{loadgenForLabel: cr.Name}

	return &batchv1.Job{
		ObjectMeta: metav1.ObjectMeta{
			Name:      loadgenJobName(cr),
			Namespace: cr.Namespace,
			Labels:    labels,
		},
		Spec: batchv1.JobSpec{
			BackoffLimit: &backoffLimit,
			Template: corev1.PodTemplateSpec{
				ObjectMeta: metav1.ObjectMeta{Labels: labels},
				Spec: corev1.PodSpec{
					RestartPolicy: corev1.RestartPolicyNever,
					Containers: []corev1.Container{{
						Name:  "loadgen",
						Image: r.LoadgenImage,
						Args:  loadgenArgs(cr),
					}},
				},
			},
		},
	}
}

// loadgenArgs renders the loadgen container flags from the CR spec. The
// loadgen binary is the image entrypoint, so the subcommand name is omitted.
func loadgenArgs(cr *v1alpha1.ScaleValidation) []string {
	return []string{
		"--url", resolveTargetURL(cr),
		"--rps", strconv.Itoa(int(cr.Spec.Load.BaseRPS)),
		"--duration", cr.Spec.SLA.Duration.String(),
		"--connection-mode", defaultLoadgenConnectionMode,
		"--target-mode", cr.Spec.Target.Mode,
		"--network-path", cr.Spec.Target.NetworkPath,
	}
}

// resolveTargetURL builds the HTTP URL the loadgen Job hits.
//
// ENG-35 assumes the workload is fronted by a Service sharing its name and
// resolves the in-cluster DNS address. Real Service discovery (selector
// matching), AutoDiscoverProbe path resolution, and Ingress host lookup
// land in ENG-36 — until then AutoDiscoverProbe and ServiceDefault both
// target "/".
func resolveTargetURL(cr *v1alpha1.ScaleValidation) string {
	path := "/"
	if cr.Spec.Target.Mode == "CustomPath" && cr.Spec.Target.CustomPath != "" {
		path = cr.Spec.Target.CustomPath
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	host := fmt.Sprintf("%s.%s.svc.cluster.local", cr.Spec.TargetRef.Name, cr.Namespace)
	return fmt.Sprintf("http://%s:%d%s", host, cr.Spec.Target.Port, path)
}
