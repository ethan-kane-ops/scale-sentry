package controller

import (
	"context"
	"errors"
	"fmt"

	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	"k8s.io/apimachinery/pkg/api/meta"
	"k8s.io/apimachinery/pkg/apis/meta/v1/unstructured"
	"k8s.io/apimachinery/pkg/labels"
	"k8s.io/apimachinery/pkg/runtime/schema"
	"sigs.k8s.io/controller-runtime/pkg/client"

	v1beta1 "github.com/ethan-kane-ops/scale-sentry/api/v1beta1"
)

// defaultTargetAPIVersion is assumed when spec.targetRef.apiVersion is
// empty. The CRD marks the field required, so this only covers objects
// written before that was enforced.
const defaultTargetAPIVersion = "apps/v1"

// scaleGVK is the GroupVersionKind of the scale subresource every
// scalable workload serves. autoscaling/v1 Scale is the stable contract
// the HorizontalPodAutoscaler itself reads, which is why the target is
// resolved through it rather than through a hardcoded workload type.
var scaleGVK = schema.GroupVersionKind{Group: "autoscaling", Version: "v1", Kind: "Scale"}

// errTargetUnresolvable marks a spec.targetRef whose apiVersion/kind
// cannot be resolved in this cluster, or that names a workload with no
// scale subresource. It is a spec error rather than a transient one, so
// callers fail the run with a diagnostic instead of requeueing forever.
var errTargetUnresolvable = errors.New("targetRef is unresolvable")

// targetGVK parses spec.targetRef into a GroupVersionKind.
func targetGVK(cr *v1beta1.ScaleValidation) (schema.GroupVersionKind, error) {
	apiVersion := cr.Spec.TargetRef.APIVersion
	if apiVersion == "" {
		apiVersion = defaultTargetAPIVersion
	}
	gv, err := schema.ParseGroupVersion(apiVersion)
	if err != nil {
		return schema.GroupVersionKind{}, fmt.Errorf("%w: parse apiVersion %q: %w", errTargetUnresolvable, apiVersion, err)
	}
	if cr.Spec.TargetRef.Kind == "" {
		return schema.GroupVersionKind{}, fmt.Errorf("%w: kind is empty", errTargetUnresolvable)
	}
	return gv.WithKind(cr.Spec.TargetRef.Kind), nil
}

// targetObject fetches the workload named by spec.targetRef as an
// unstructured object. Unstructured rather than a typed appsv1.Deployment
// so any scalable kind resolves; callers that only need replica counts
// should prefer targetSelector, which goes through the scale subresource.
func (r *ScaleValidationReconciler) targetObject(ctx context.Context, cr *v1beta1.ScaleValidation) (*unstructured.Unstructured, error) {
	gvk, err := targetGVK(cr)
	if err != nil {
		return nil, err
	}
	obj := &unstructured.Unstructured{}
	obj.SetGroupVersionKind(gvk)
	key := client.ObjectKey{Namespace: cr.Namespace, Name: cr.Spec.TargetRef.Name}
	if err := r.Get(ctx, key, obj); err != nil {
		return nil, classifyTargetError(cr, err)
	}
	return obj, nil
}

// targetSelector reads the target's scale subresource and returns
// status.selector, the label selector the workload's own controller uses
// to own its pods. This is the same value the HPA reads, so pods counted
// against it are exactly the pods the HPA is scaling.
func (r *ScaleValidationReconciler) targetSelector(ctx context.Context, cr *v1beta1.ScaleValidation) (string, error) {
	gvk, err := targetGVK(cr)
	if err != nil {
		return "", err
	}
	parent := &unstructured.Unstructured{}
	parent.SetGroupVersionKind(gvk)
	parent.SetNamespace(cr.Namespace)
	parent.SetName(cr.Spec.TargetRef.Name)

	// The unstructured client requires the subresource object to be
	// unstructured too, so Scale is read field-wise rather than decoded
	// into autoscalingv1.Scale.
	scale := &unstructured.Unstructured{}
	scale.SetGroupVersionKind(scaleGVK)
	if err := r.SubResource("scale").Get(ctx, parent, scale); err != nil {
		return "", classifyTargetError(cr, err)
	}

	selector, _, err := unstructured.NestedString(scale.Object, "status", "selector")
	if err != nil {
		return "", fmt.Errorf("read scale status.selector for %s %s: %w", cr.Spec.TargetRef.Kind, cr.Spec.TargetRef.Name, err)
	}
	if selector == "" {
		return "", fmt.Errorf("%w: %s %s reports an empty scale status.selector, so its pods cannot be identified",
			errTargetUnresolvable, cr.Spec.TargetRef.Kind, cr.Spec.TargetRef.Name)
	}
	return selector, nil
}

// classifyTargetError converts an API error against the target into
// errTargetUnresolvable where the cause is the spec rather than the
// cluster being mid-rollout. A kind the apiserver does not serve, or one
// the manager has no RBAC for, will never resolve on a retry; NotFound is
// deliberately left alone so the readiness gate can keep waiting for a
// workload the user has not applied yet.
func classifyTargetError(cr *v1beta1.ScaleValidation, err error) error {
	ref := cr.Spec.TargetRef
	switch {
	case meta.IsNoMatchError(err):
		return fmt.Errorf("%w: cluster serves no %s in %s: %w", errTargetUnresolvable, ref.Kind, ref.APIVersion, err)
	case apierrors.IsForbidden(err):
		return fmt.Errorf("%w: not permitted to read %s %s or its scale subresource, extend the manager ClusterRole: %w",
			errTargetUnresolvable, ref.Kind, ref.Name, err)
	default:
		return err
	}
}

// targetPodsReady counts the target's ready pods via the scale
// subresource's selector. Returns (false, 0, nil) when the workload does
// not exist yet: applying a ScaleValidation before its workload is a
// normal ordering, not an error.
func (r *ScaleValidationReconciler) targetPodsReady(ctx context.Context, cr *v1beta1.ScaleValidation) (bool, int32, error) {
	selector, err := r.targetSelector(ctx, cr)
	switch {
	case apierrors.IsNotFound(err):
		return false, 0, nil
	case err != nil:
		return false, 0, err
	}

	sel, err := labels.Parse(selector)
	if err != nil {
		return false, 0, fmt.Errorf("parse scale status.selector %q: %w", selector, err)
	}
	var pods corev1.PodList
	if err := r.List(ctx, &pods, client.InNamespace(cr.Namespace), client.MatchingLabelsSelector{Selector: sel}); err != nil {
		return false, 0, fmt.Errorf("list target pods: %w", err)
	}
	var ready int32
	for i := range pods.Items {
		if podReady(&pods.Items[i]) {
			ready++
		}
	}
	return ready >= 1, ready, nil
}

// podReady reports whether p carries a PodReady condition with status
// True. Pods that are Running but failing their readiness probe are not
// ready, which is the distinction the loadgen gate cares about.
func podReady(p *corev1.Pod) bool {
	for _, c := range p.Status.Conditions {
		if c.Type == corev1.PodReady {
			return c.Status == corev1.ConditionTrue
		}
	}
	return false
}
