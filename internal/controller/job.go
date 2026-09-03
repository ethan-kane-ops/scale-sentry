package controller

import (
	"context"
	"fmt"
	"strconv"
	"strings"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/runtime"

	v1beta1 "github.com/ethan-kane-ops/scale-sentry/api/v1beta1"
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
func loadgenJobName(cr *v1beta1.ScaleValidation) string {
	return cr.Name + "-loadgen"
}

// buildLoadgenJob constructs the validation Job: the loadgen container plus
// the observer native sidecar, sharing an emptyDir for the result file.
// BackoffLimit is 0, a load run is a measurement, not retryable work.
func (r *ScaleValidationReconciler) buildLoadgenJob(cr *v1beta1.ScaleValidation, url string) (*batchv1.Job, error) {
	backoffLimit := int32(0)
	grace := int64(jobGracePeriodSeconds)
	sidecarRestart := corev1.ContainerRestartPolicyAlways
	labels := map[string]string{loadgenForLabel: cr.Name}

	loadgenMounts := []corev1.VolumeMount{{Name: runVolumeName, MountPath: runVolumePath}}
	observerMounts := []corev1.VolumeMount{{Name: runVolumeName, MountPath: runVolumePath}}

	obsArgs, err := r.observerArgs(cr)
	if err != nil {
		return nil, err
	}
	containerSC := restrictedContainerSecurityContext()
	podSC := restrictedPodSecurityContext()
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

	lgArgs, err := loadgenArgs(cr, url, caBundlePath)
	if err != nil {
		return nil, err
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
					SecurityContext:               podSC,
					ImagePullSecrets:              imagePullSecretRefs(r.ImagePullSecrets),
					Volumes:                       volumes,
					// The observer is a native sidecar, a restartPolicy:
					// Always init container. It starts before the load
					// generator and is SIGTERM'd once loadgen exits, which
					// is its signal to finalize and print the Report.
					InitContainers: []corev1.Container{{
						Name:            observerContainerName,
						Image:           r.ObserverImage,
						Args:            obsArgs,
						RestartPolicy:   &sidecarRestart,
						SecurityContext: containerSC,
						VolumeMounts:    observerMounts,
					}},
					Containers: []corev1.Container{{
						Name:            loadgenContainerName,
						Image:           r.LoadgenImage,
						Args:            lgArgs,
						SecurityContext: containerSC,
						VolumeMounts:    loadgenMounts,
					}},
				},
			},
		},
	}, nil
}

// loadgenArgs renders the loadgen container flags. The loadgen binary is
// the image entrypoint, so the subcommand name is omitted. caBundlePath is
// non-empty only when the CR's TLS block references a CA ConfigMap.
//
// When the CR carries a load profile (warmup or non-Constant pattern),
// the controller marshals the resolved phase list into --phases JSON;
// otherwise the legacy --rps/--duration flags are passed so existing
// samples and the simple-Constant case keep working unchanged.
func loadgenArgs(cr *v1beta1.ScaleValidation, url, caBundlePath string) ([]string, error) {
	args := []string{
		"--url", url,
		"--connection-mode", defaultLoadgenConnectionMode,
		"--target-mode", string(cr.Spec.Target.Mode),
		"--network-path", string(cr.Spec.Target.NetworkPath),
		"--result-file", resultFilePath,
	}
	if proto := cr.Spec.Target.Protocol; proto != "" {
		args = append(args, "--protocol", string(proto))
	}
	if g := cr.Spec.Target.GRPC; g != nil && g.Service != "" {
		args = append(args, "--grpc-service", g.Service)
	}
	phases, err := buildPhases(cr)
	if err != nil {
		return nil, err
	}
	if phases != nil {
		js, err := phasesJSON(phases)
		if err != nil {
			return nil, err
		}
		args = append(args, "--phases", js)
	} else {
		args = append(args,
			"--rps", strconv.Itoa(int(cr.Spec.Load.BaseRPS)),
			"--duration", cr.Spec.SLA.Duration.String(),
		)
	}
	if tls := cr.Spec.Target.TLS; tls != nil {
		if tls.InsecureSkipVerify {
			args = append(args, "--tls-insecure-skip-verify")
		}
	}
	if caBundlePath != "" {
		args = append(args, "--tls-ca-bundle", caBundlePath)
	}
	return args, nil
}

// caBundleRef returns the ConfigMapKeyRef from the CR's TLS spec, or nil.
func caBundleRef(cr *v1beta1.ScaleValidation) *v1beta1.ConfigMapKeyRef {
	if cr.Spec.Target.TLS == nil || cr.Spec.Target.TLS.CABundle == nil {
		return nil
	}
	return &cr.Spec.Target.TLS.CABundle.ConfigMapRef
}

// observerArgs renders the observer sidecar flags. The target's
// GroupVersionResource is resolved here rather than in the observer: the
// manager already runs a RESTMapper, so the sidecar needs no discovery
// permissions of its own to read the workload's scale subresource.
func (r *ScaleValidationReconciler) observerArgs(cr *v1beta1.ScaleValidation) ([]string, error) {
	gvk, err := targetGVK(cr)
	if err != nil {
		return nil, err
	}
	mapping, err := r.RESTMapper().RESTMapping(gvk.GroupKind(), gvk.Version)
	if err != nil {
		return nil, fmt.Errorf("%w: no REST mapping for %s: %w", errTargetUnresolvable, gvk, err)
	}
	return []string{
		"--target-name", cr.Spec.TargetRef.Name,
		"--namespace", cr.Namespace,
		"--target-kind", gvk.Kind,
		"--target-group", mapping.Resource.Group,
		"--target-version", mapping.Resource.Version,
		"--target-resource", mapping.Resource.Resource,
		"--sla", cr.Spec.SLA.Duration.String(),
		"--result-file", resultFilePath,
	}, nil
}

// resolveTargetURL builds the HTTP URL the loadgen Job hits. ServiceDefault
// and CustomPath are resolved purely from the spec; AutoDiscoverProbe reads
// the target Deployment's readiness probe via the probe analyzer.
//
// The default workload is assumed to be fronted by a Service sharing its
// name; .spec.target.host overrides that when set, which is the entry
// point for Gateway / Ingress runs that route through an edge address
// rather than the in-cluster Service DNS.
func (r *ScaleValidationReconciler) resolveTargetURL(ctx context.Context, cr *v1beta1.ScaleValidation) (string, error) {
	host := cr.Spec.Target.Host
	if host == "" {
		host = fmt.Sprintf("%s.%s.svc.cluster.local", cr.Spec.TargetRef.Name, cr.Namespace)
	}
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

// discoverProbe fetches the target workload named by spec.targetRef and
// resolves its first container's readiness probe. The pod template lives
// at the same path on every workload kind that has one (Deployment,
// StatefulSet, ReplicaSet, DaemonSet), so the lookup is done on the
// unstructured object rather than against a single typed kind.
func (r *ScaleValidationReconciler) discoverProbe(ctx context.Context, cr *v1beta1.ScaleValidation) (probe.Spec, error) {
	obj, err := r.targetObject(ctx, cr)
	if err != nil {
		return probe.Spec{}, err
	}
	ref := cr.Spec.TargetRef
	raw, found, err := unstructured.NestedSlice(obj.Object, "spec", "template", "spec", "containers")
	if err != nil {
		return probe.Spec{}, fmt.Errorf("read pod template of %s %s: %w", ref.Kind, ref.Name, err)
	}
	if !found || len(raw) == 0 {
		return probe.Spec{}, fmt.Errorf("%s %s has no pod template containers, AutoDiscoverProbe needs one", ref.Kind, ref.Name)
	}
	fields, ok := raw[0].(map[string]any)
	if !ok {
		return probe.Spec{}, fmt.Errorf("%s %s has a malformed first container", ref.Kind, ref.Name)
	}
	var container corev1.Container
	if err := runtime.DefaultUnstructuredConverter.FromUnstructured(fields, &container); err != nil {
		return probe.Spec{}, fmt.Errorf("decode first container of %s %s: %w", ref.Kind, ref.Name, err)
	}
	return probe.DiscoverFromContainer(container)
}

// restrictedPodSecurityContext satisfies the PodSecurityAdmission Restricted
// profile: non-root user, runtime-default seccomp.
func restrictedPodSecurityContext() *corev1.PodSecurityContext {
	nonRoot := true
	return &corev1.PodSecurityContext{
		RunAsNonRoot:   &nonRoot,
		SeccompProfile: &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
}

// restrictedContainerSecurityContext satisfies the PodSecurityAdmission
// Restricted profile per container: drop ALL caps, no privilege escalation,
// read-only root filesystem. The shared run volume is writable for both the
// loadgen result file and the observer's reads.
func restrictedContainerSecurityContext() *corev1.SecurityContext {
	nonRoot := true
	noEsc := false
	readOnly := true
	return &corev1.SecurityContext{
		AllowPrivilegeEscalation: &noEsc,
		RunAsNonRoot:             &nonRoot,
		ReadOnlyRootFilesystem:   &readOnly,
		Capabilities:             &corev1.Capabilities{Drop: []corev1.Capability{"ALL"}},
		SeccompProfile:           &corev1.SeccompProfile{Type: corev1.SeccompProfileTypeRuntimeDefault},
	}
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

// imagePullSecretRefs converts the configured pull-secret names into the
// reference list a PodSpec wants. Returns nil for an empty configuration so
// the field is omitted entirely rather than serialised as an empty list.
func imagePullSecretRefs(names []string) []corev1.LocalObjectReference {
	if len(names) == 0 {
		return nil
	}
	refs := make([]corev1.LocalObjectReference, 0, len(names))
	for _, n := range names {
		refs = append(refs, corev1.LocalObjectReference{Name: n})
	}
	return refs
}
