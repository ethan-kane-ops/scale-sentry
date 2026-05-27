package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	appsv1 "k8s.io/api/apps/v1"
	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1alpha1 "github.com/ethan-kane-ops/scale-sentry/api/v1alpha1"
	"github.com/ethan-kane-ops/scale-sentry/internal/analyzer/probe"
)

const (
	// loadgenForLabel marks a Job (and its pods) as belonging to a CR.
	loadgenForLabel = "validation.scale-sentry.ek.co/loadgen-for"
	// defaultLoadgenConnectionMode is passed to every loadgen run.
	defaultLoadgenConnectionMode = "KeepAlive"

	loadgenContainerName  = "loadgen"
	observerContainerName = "observer"

	// runVolume is the emptyDir shared by the loadgen and observer
	// containers: loadgen writes its result file, the observer reads it.
	runVolumeName  = "scale-sentry-run"
	runVolumePath  = "/run/scale-sentry"
	resultFilePath = runVolumePath + "/result.json"

	// tlsCAVolume is the read-only mount for the user-supplied PEM bundle
	// (CABundle ConfigMapRef). Mounted only when the CR sets a CA source.
	tlsCAVolumeName = "scale-sentry-tls-ca"
	tlsCAMountPath  = "/etc/scale-sentry/tls-ca"

	// jobGracePeriodSeconds gives the observer sidecar time to finalize
	// (final cgroup scrape, analysis, report) after the load run exits.
	jobGracePeriodSeconds = 45
)

// loadgenJobName is the deterministic name of the loadgen Job for cr, so
// reconciliation can find an existing run instead of spawning duplicates.
func loadgenJobName(cr *v1alpha1.ScaleValidation) string {
	return cr.Name + "-loadgen"
}

// buildLoadgenJob constructs the validation Job: the loadgen container plus
// the observer native sidecar, sharing an emptyDir for the result file.
// BackoffLimit is 0, a load run is a measurement, not retryable work.
func (r *ScaleValidationReconciler) buildLoadgenJob(cr *v1alpha1.ScaleValidation, url string) *batchv1.Job {
	backoffLimit := int32(0)
	grace := int64(jobGracePeriodSeconds)
	sidecarRestart := corev1.ContainerRestartPolicyAlways
	labels := map[string]string{loadgenForLabel: cr.Name}

	loadgenMounts := []corev1.VolumeMount{{Name: runVolumeName, MountPath: runVolumePath}}
	observerMounts := []corev1.VolumeMount{{Name: runVolumeName, MountPath: runVolumePath}}
	volumes := []corev1.Volume{{
		Name:         runVolumeName,
		VolumeSource: corev1.VolumeSource{EmptyDir: &corev1.EmptyDirVolumeSource{}},
	}}

	caBundlePath := ""
	if ca := caBundleRef(cr); ca != nil {
		caBundlePath = tlsCAMountPath + "/" + ca.Key
		volumes = append(volumes, corev1.Volume{
			Name: tlsCAVolumeName,
			VolumeSource: corev1.VolumeSource{
				ConfigMap: &corev1.ConfigMapVolumeSource{
					LocalObjectReference: corev1.LocalObjectReference{Name: ca.Name},
					Items: []corev1.KeyToPath{
						{Key: ca.Key, Path: ca.Key},
					},
				},
			},
		})
		loadgenMounts = append(loadgenMounts, corev1.VolumeMount{
			Name: tlsCAVolumeName, MountPath: tlsCAMountPath, ReadOnly: true,
		})
	}

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
					RestartPolicy:                 corev1.RestartPolicyNever,
					ServiceAccountName:            r.ObserverServiceAccount,
					TerminationGracePeriodSeconds: &grace,
					Volumes:                       volumes,
					// The observer is a native sidecar, a restartPolicy:
					// Always init container. It starts before the load
					// generator and is SIGTERM'd once loadgen exits, which
					// is its signal to finalize and print the Report.
					InitContainers: []corev1.Container{{
						Name:          observerContainerName,
						Image:         r.ObserverImage,
						Args:          observerArgs(cr),
						RestartPolicy: &sidecarRestart,
						VolumeMounts:  observerMounts,
					}},
					Containers: []corev1.Container{{
						Name:         loadgenContainerName,
						Image:        r.LoadgenImage,
						Args:         loadgenArgs(cr, url, caBundlePath),
						VolumeMounts: loadgenMounts,
					}},
				},
			},
		},
	}
}

// loadgenArgs renders the loadgen container flags. The loadgen binary is
// the image entrypoint, so the subcommand name is omitted. caBundlePath is
// non-empty only when the CR's TLS block references a CA ConfigMap.
func loadgenArgs(cr *v1alpha1.ScaleValidation, url, caBundlePath string) []string {
	args := []string{
		"--url", url,
		"--rps", strconv.Itoa(int(cr.Spec.Load.BaseRPS)),
		"--duration", cr.Spec.SLA.Duration.String(),
		"--connection-mode", defaultLoadgenConnectionMode,
		"--target-mode", cr.Spec.Target.Mode,
		"--network-path", cr.Spec.Target.NetworkPath,
		"--result-file", resultFilePath,
	}
	if tls := cr.Spec.Target.TLS; tls != nil {
		if tls.InsecureSkipVerify {
			args = append(args, "--tls-insecure-skip-verify")
		}
	}
	if caBundlePath != "" {
		args = append(args, "--tls-ca-bundle", caBundlePath)
	}
	return args
}

// caBundleRef returns the ConfigMapKeyRef from the CR's TLS spec, or nil.
func caBundleRef(cr *v1alpha1.ScaleValidation) *v1alpha1.ConfigMapKeyRef {
	if cr.Spec.Target.TLS == nil || cr.Spec.Target.TLS.CABundle == nil {
		return nil
	}
	return &cr.Spec.Target.TLS.CABundle.ConfigMapRef
}

// observerArgs renders the observer sidecar flags.
func observerArgs(cr *v1alpha1.ScaleValidation) []string {
	return []string{
		"--target-name", cr.Spec.TargetRef.Name,
		"--namespace", cr.Namespace,
		"--sla", cr.Spec.SLA.Duration.String(),
		"--result-file", resultFilePath,
	}
}

// resolveTargetURL builds the HTTP URL the loadgen Job hits. ServiceDefault
// and CustomPath are resolved purely from the spec; AutoDiscoverProbe reads
// the target Deployment's readiness probe via the probe analyzer.
//
// The workload is assumed to be fronted by a Service sharing its name;
// real Service discovery and Ingress host resolution remain future work.
func (r *ScaleValidationReconciler) resolveTargetURL(ctx context.Context, cr *v1alpha1.ScaleValidation) (string, error) {
	host := fmt.Sprintf("%s.%s.svc.cluster.local", cr.Spec.TargetRef.Name, cr.Namespace)
	port := cr.Spec.Target.Port
	scheme := "http"

	switch cr.Spec.Target.Mode {
	case "CustomPath":
		return targetURL(scheme, host, port, cr.Spec.Target.CustomPath), nil
	case "AutoDiscoverProbe":
		spec, err := r.discoverProbe(ctx, cr)
		if err != nil {
			return "", fmt.Errorf("auto-discover probe: %w", err)
		}
		if spec.Port != 0 {
			port = spec.Port
		}
		if strings.EqualFold(spec.Scheme, "HTTPS") {
			scheme = "https"
		}
		return targetURL(scheme, host, port, spec.Path), nil
	default: // ServiceDefault
		return targetURL(scheme, host, port, "/"), nil
	}
}

// discoverProbe fetches the target Deployment and resolves its first
// container's readiness probe.
func (r *ScaleValidationReconciler) discoverProbe(ctx context.Context, cr *v1alpha1.ScaleValidation) (probe.Spec, error) {
	var deploy appsv1.Deployment
	key := types.NamespacedName{Namespace: cr.Namespace, Name: cr.Spec.TargetRef.Name}
	if err := r.Get(ctx, key, &deploy); err != nil {
		return probe.Spec{}, fmt.Errorf("get deployment %s: %w", cr.Spec.TargetRef.Name, err)
	}
	containers := deploy.Spec.Template.Spec.Containers
	if len(containers) == 0 {
		return probe.Spec{}, fmt.Errorf("deployment %s has no containers", cr.Spec.TargetRef.Name)
	}
	return probe.DiscoverFromContainer(containers[0])
}

// targetURL assembles an HTTP URL, normalizing the path to a leading slash.
func targetURL(scheme, host string, port int32, path string) string {
	if path == "" {
		path = "/"
	}
	if !strings.HasPrefix(path, "/") {
		path = "/" + path
	}
	return fmt.Sprintf("%s://%s:%d%s", scheme, host, port, path)
}
