//go:build e2e

// Scenario e2e tests beyond the full-verdict happy path. Each scenario gets
// its own GenerateName namespace so runs cannot interfere; none of them
// needs metrics-server (no verdict assertion), which keeps them minutes
// cheaper than TestE2E_FullVerdict.
package e2e

import (
	"context"
	"testing"
	"time"

	batchv1 "k8s.io/api/batch/v1"
	corev1 "k8s.io/api/core/v1"
	apierrors "k8s.io/apimachinery/pkg/api/errors"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/apimachinery/pkg/types"

	v1beta1 "github.com/ethan-kane-ops/scale-sentry/api/v1beta1"
)

const (
	scenarioTimeout  = 6 * time.Minute
	jobAppearAfter   = 3 * time.Minute
	cleanupGoneAfter = 2 * time.Minute
	shadowAfter      = 2 * time.Minute
)

// TestE2E_FinalizerCleansUpJobs asserts that deleting a ScaleValidation
// mid-run tears down the loadgen Job (observer rides it as a sidecar)
// instead of leaving it burning traffic until its own completion.
func TestE2E_FinalizerCleansUpJobs(t *testing.T) {
	c := newE2EClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ss-e2e-fin-"}}
	mustCreate(t, c, ctx, ns)
	t.Cleanup(func() { _ = c.Delete(context.Background(), ns) })
	applyObserverRBAC(t, c, ctx, ns.Name)

	labels := map[string]string{"app": "target"}
	mustCreate(t, c, ctx, targetDeployment(ns.Name, labels))
	mustCreate(t, c, ctx, targetService(ns.Name, labels))
	if err := waitForDeploymentReady(ctx, c, ns.Name, "target", deploymentReadyAfter); err != nil {
		t.Fatalf("target not ready: %v", err)
	}

	cr := &v1beta1.ScaleValidation{
		ObjectMeta: metav1.ObjectMeta{Name: "finalizer", Namespace: ns.Name},
		Spec: v1beta1.ScaleValidationSpec{
			TargetRef: v1beta1.CrossVersionObjectReference{
				APIVersion: "apps/v1", Kind: "Deployment", Name: "target",
			},
			// Long SLA keeps the run in-flight so the delete lands mid-run.
			SLA:    metav1.Duration{Duration: 5 * time.Minute},
			Target: v1beta1.TargetConfig{Mode: "ServiceDefault", Port: targetPort, NetworkPath: "ClusterIP"},
			Load:   v1beta1.LoadConfig{BaseRPS: 5},
		},
	}
	mustCreate(t, c, ctx, cr)

	jobKey := types.NamespacedName{Namespace: ns.Name, Name: cr.Name + "-loadgen"}
	if err := waitFor(ctx, jobAppearAfter, func() (bool, error) {
		var job batchv1.Job
		err := c.Get(ctx, jobKey, &job)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return err == nil, err
	}); err != nil {
		dumpCR(t, c, ns.Name, cr.Name)
		t.Fatalf("loadgen Job never appeared: %v", err)
	}

	if err := c.Delete(ctx, cr); err != nil {
		t.Fatalf("delete CR: %v", err)
	}

	if err := waitFor(ctx, cleanupGoneAfter, func() (bool, error) {
		err := c.Get(ctx, types.NamespacedName{Namespace: ns.Name, Name: cr.Name}, &v1beta1.ScaleValidation{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}); err != nil {
		t.Fatalf("CR still present after delete (finalizer stuck?): %v", err)
	}

	if err := waitFor(ctx, cleanupGoneAfter, func() (bool, error) {
		err := c.Get(ctx, jobKey, &batchv1.Job{})
		if apierrors.IsNotFound(err) {
			return true, nil
		}
		return false, err
	}); err != nil {
		t.Fatalf("loadgen Job survived CR delete: %v", err)
	}
}

// TestE2E_AnnotationBridgeCreatesShadowCR asserts that annotating a
// Deployment with validation.scale-sentry.ek.co/enabled=true provisions
// an owner-referenced shadow ScaleValidation without any manifest.
func TestE2E_AnnotationBridgeCreatesShadowCR(t *testing.T) {
	c := newE2EClient(t)
	ctx, cancel := context.WithTimeout(context.Background(), scenarioTimeout)
	defer cancel()

	ns := &corev1.Namespace{ObjectMeta: metav1.ObjectMeta{GenerateName: "ss-e2e-shadow-"}}
	mustCreate(t, c, ctx, ns)
	t.Cleanup(func() { _ = c.Delete(context.Background(), ns) })
	applyObserverRBAC(t, c, ctx, ns.Name)

	labels := map[string]string{"app": "target"}
	deploy := targetDeployment(ns.Name, labels)
	deploy.Annotations = map[string]string{
		"validation.scale-sentry.ek.co/enabled": "true",
		"validation.scale-sentry.ek.co/sla":     "90s",
	}
	mustCreate(t, c, ctx, deploy)
	mustCreate(t, c, ctx, targetService(ns.Name, labels))

	shadowKey := types.NamespacedName{Namespace: ns.Name, Name: deploy.Name + "-shadow"}
	var shadow v1beta1.ScaleValidation
	if err := waitFor(ctx, shadowAfter, func() (bool, error) {
		err := c.Get(ctx, shadowKey, &shadow)
		if apierrors.IsNotFound(err) {
			return false, nil
		}
		return err == nil, err
	}); err != nil {
		t.Fatalf("shadow ScaleValidation never appeared: %v", err)
	}

	if got := shadow.Spec.SLA.Duration; got != 90*time.Second {
		t.Errorf("shadow SLA = %s, want 90s (from annotation)", got)
	}
	if len(shadow.OwnerReferences) != 1 || shadow.OwnerReferences[0].Kind != "Deployment" {
		t.Errorf("shadow owner references = %+v, want single Deployment owner", shadow.OwnerReferences)
	}
}
