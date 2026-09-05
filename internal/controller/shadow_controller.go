package controller

import (
	"context"
	"fmt"
	"strconv"
	"time"

	appsv1 "k8s.io/api/apps/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/runtime"
	"k8s.io/apimachinery/pkg/types"
	ctrl "sigs.k8s.io/controller-runtime"
	"sigs.k8s.io/controller-runtime/pkg/client"
	"sigs.k8s.io/controller-runtime/pkg/controller/controllerutil"
	logf "sigs.k8s.io/controller-runtime/pkg/log"
	"sigs.k8s.io/controller-runtime/pkg/predicate"

	v1beta1 "github.com/ethan-kane-ops/scale-sentry/api/v1beta1"
)

// Annotations on a Deployment that opt it into an auto-generated
// ("shadow") ScaleValidation. Only the enable annotation is required.
const (
	shadowEnableAnnotation = "validation.scale-sentry.ek.co/enabled"
	shadowPortAnnotation   = "validation.scale-sentry.ek.co/port"
	shadowSLAAnnotation    = "validation.scale-sentry.ek.co/sla"
	shadowRPSAnnotation    = "validation.scale-sentry.ek.co/base-rps"
)

// Defaults applied when the optional shadow annotations are absent.
const (
	shadowDefaultPort    = int32(80)
	shadowDefaultSLA     = 5 * time.Minute
	shadowDefaultBaseRPS = int32(50)
)

// DeploymentShadowReconciler creates a ScaleValidation for any Deployment
// annotated with shadowEnableAnnotation.
type DeploymentShadowReconciler struct {
	client.Client
	Scheme *runtime.Scheme
}

//+kubebuilder:rbac:groups=apps,resources=deployments,verbs=get;list;watch

// Reconcile ensures an opted-in Deployment has a shadow ScaleValidation. The
// shadow CR is owner-referenced to the Deployment, so it is garbage-collected
// when the Deployment is deleted. The CR is created once and never updated, // users edit the spawned CR directly to re-tune a run.
func (r *DeploymentShadowReconciler) Reconcile(ctx context.Context, req ctrl.Request) (ctrl.Result, error) {
	log := logf.FromContext(ctx)

	var deploy appsv1.Deployment
	if err := r.Get(ctx, req.NamespacedName, &deploy); err != nil {
		return ctrl.Result{}, client.IgnoreNotFound(err)
	}
	if deploy.Annotations[shadowEnableAnnotation] != "true" {
		return ctrl.Result{}, nil
	}

	name := deploy.Name + "-shadow"
	key := types.NamespacedName{Namespace: deploy.Namespace, Name: name}
	err := r.Get(ctx, key, &v1beta1.ScaleValidation{})
	switch {
	case err == nil:
		return ctrl.Result{}, nil
	case !apierrors.IsNotFound(err):
		return ctrl.Result{}, fmt.Errorf("get shadow scalevalidation: %w", err)
	}

	sv, err := shadowScaleValidation(&deploy, name)
	if err != nil {
		// Malformed annotations are a user error, log and stop rather than
		// hot-loop. A corrected annotation re-triggers a fresh reconcile.
		log.Error(err, "invalid shadow annotations", "deployment", deploy.Name)
		return ctrl.Result{}, nil
	}
	if err := controllerutil.SetControllerReference(&deploy, sv, r.Scheme); err != nil {
		return ctrl.Result{}, fmt.Errorf("set owner reference: %w", err)
	}
	if err := r.Create(ctx, sv); err != nil {
		return ctrl.Result{}, fmt.Errorf("create shadow scalevalidation: %w", err)
	}
	log.Info("created shadow ScaleValidation", "name", name, "deployment", deploy.Name)
	return ctrl.Result{}, nil
}

// shadowScaleValidation builds the CR for deploy, reading optional tuning
// annotations and falling back to the shadowDefault* values.
func shadowScaleValidation(deploy *appsv1.Deployment, name string) (*v1beta1.ScaleValidation, error) {
	port := shadowDefaultPort
	if v, ok := deploy.Annotations[shadowPortAnnotation]; ok {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("annotation %s: %w", shadowPortAnnotation, err)
		}
		port = int32(n)
	}

	sla := shadowDefaultSLA
	if v, ok := deploy.Annotations[shadowSLAAnnotation]; ok {
		d, err := time.ParseDuration(v)
		if err != nil {
			return nil, fmt.Errorf("annotation %s: %w", shadowSLAAnnotation, err)
		}
		sla = d
	}

	baseRPS := shadowDefaultBaseRPS
	if v, ok := deploy.Annotations[shadowRPSAnnotation]; ok {
		n, err := strconv.ParseInt(v, 10, 32)
		if err != nil {
			return nil, fmt.Errorf("annotation %s: %w", shadowRPSAnnotation, err)
		}
		baseRPS = int32(n)
	}

	return &v1beta1.ScaleValidation{
		ObjectMeta: metav1.ObjectMeta{Name: name, Namespace: deploy.Namespace},
		Spec: v1beta1.ScaleValidationSpec{
			TargetRef: v1beta1.CrossVersionObjectReference{
				APIVersion: "apps/v1",
				Kind:       "Deployment",
				Name:       deploy.Name,
			},
			SLA: metav1.Duration{Duration: sla},
			Target: v1beta1.TargetConfig{
				Mode:        "ServiceDefault",
				Port:        port,
				NetworkPath: "ClusterIP",
			},
			Load: v1beta1.LoadConfig{
				BaseRPS: baseRPS,
			},
		},
	}, nil
}

// SetupWithManager registers the reconciler, filtered to Deployments that
// carry the enable annotation so unrelated Deployment churn is ignored.
func (r *DeploymentShadowReconciler) SetupWithManager(mgr ctrl.Manager) error {
	enabled := predicate.NewPredicateFuncs(func(obj client.Object) bool {
		return obj.GetAnnotations()[shadowEnableAnnotation] == "true"
	})
	return ctrl.NewControllerManagedBy(mgr).
		For(&appsv1.Deployment{}).
		WithEventFilter(enabled).
		Named("deployment-shadow").
		Complete(r)
}
