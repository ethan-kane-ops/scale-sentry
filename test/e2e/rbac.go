//go:build e2e

package e2e

import (
	"context"
	"testing"

	corev1 "k8s.io/api/core/v1"
	rbacv1 "k8s.io/api/rbac/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"sigs.k8s.io/controller-runtime/pkg/client"
)

const (
	observerSAName       = "scale-sentry-observer"
	observerCAdvisorRole = "scale-sentry-observer-cadvisor"
)

// applyObserverRBAC mirrors charts/scale-sentry/templates/observer-rbac.yaml
// and observer-clusterrolebinding.yaml into the given namespace. The
// chart only installs observer RBAC in its release namespace; runs in
// arbitrary namespaces (like the per-test ns here) need it laid down
// separately, which mirrors what end users do.
//
// The ClusterRole + ClusterRoleBinding for kubelet cAdvisor scraping are
// shared across test runs (cluster-scoped); creates are best-effort and
// the IsAlreadyExists path is treated as success by mustCreate.
func applyObserverRBAC(t *testing.T, c client.Client, ctx context.Context, ns string) {
	t.Helper()
	mustCreate(t, c, ctx, &corev1.ServiceAccount{
		ObjectMeta: metav1.ObjectMeta{Name: observerSAName, Namespace: ns},
	})
	mustCreate(t, c, ctx, &rbacv1.Role{
		ObjectMeta: metav1.ObjectMeta{Name: observerSAName, Namespace: ns},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{"apps"}, Resources: []string{"deployments/scale", "replicasets/scale", "statefulsets/scale"}, Verbs: []string{"get"}},
			{APIGroups: []string{""}, Resources: []string{"pods"}, Verbs: []string{"get", "list"}},
			{APIGroups: []string{"discovery.k8s.io"}, Resources: []string{"endpointslices"}, Verbs: []string{"list", "watch"}},
			{APIGroups: []string{"autoscaling"}, Resources: []string{"horizontalpodautoscalers"}, Verbs: []string{"get", "list"}},
		},
	})
	mustCreate(t, c, ctx, &rbacv1.RoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: observerSAName, Namespace: ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "Role", Name: observerSAName},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: observerSAName, Namespace: ns}},
	})
	mustCreate(t, c, ctx, &rbacv1.ClusterRole{
		ObjectMeta: metav1.ObjectMeta{Name: observerCAdvisorRole},
		Rules: []rbacv1.PolicyRule{
			{APIGroups: []string{""}, Resources: []string{"nodes/proxy"}, Verbs: []string{"get"}},
		},
	})
	mustCreate(t, c, ctx, &rbacv1.ClusterRoleBinding{
		ObjectMeta: metav1.ObjectMeta{Name: observerCAdvisorRole + "-" + ns},
		RoleRef:    rbacv1.RoleRef{APIGroup: "rbac.authorization.k8s.io", Kind: "ClusterRole", Name: observerCAdvisorRole},
		Subjects:   []rbacv1.Subject{{Kind: "ServiceAccount", Name: observerSAName, Namespace: ns}},
	})
}
