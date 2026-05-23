package observer

import (
	"bytes"
	"context"
	"fmt"
	"io"

	corev1 "k8s.io/api/core/v1"

	"github.com/ethan-kane-ops/scale-sentry/internal/analyzer/cgroup"
)

// scrapeCgroup pulls CFS counters for pod's first container from the
// kubelet's cAdvisor metrics endpoint, via the apiserver proxy so the
// observer never needs a direct network path to the node. This replaces
// the original `pods/exec` + `cat /sys/fs/cgroup/cpu.stat` approach, which
// (1) breaks on distroless target images that lack `cat`, and (2) requires
// `pods/exec` RBAC — a high-privilege verb the observer SA otherwise has
// no use for.
func (s *Session) scrapeCgroup(ctx context.Context, pod *corev1.Pod) (cgroup.Stat, error) {
	if pod == nil || len(pod.Spec.Containers) == 0 {
		return cgroup.Stat{}, fmt.Errorf("no pod or container to scrape")
	}
	if pod.Spec.NodeName == "" {
		return cgroup.Stat{}, fmt.Errorf("pod %s not yet scheduled to a node", pod.Name)
	}
	body, err := s.fetchCAdvisor(ctx, pod.Spec.NodeName)
	if err != nil {
		return cgroup.Stat{}, err
	}
	return cgroup.ParseCAdvisor(bytes.NewReader(body), pod.Name, pod.Namespace, pod.Spec.Containers[0].Name)
}

// cadvisorReader is the indirection used by [Session.scrapeCgroup] to
// reach the kubelet's /metrics/cadvisor endpoint. Production runs go via
// the apiserver proxy; tests inject a stub that returns a canned scrape
// body so the cgroup parser can be exercised end-to-end without a kubelet.
type cadvisorReader func(ctx context.Context, node string) (io.ReadCloser, error)

func (s *Session) fetchCAdvisor(ctx context.Context, node string) ([]byte, error) {
	open := s.cadvisorOpen
	if open == nil {
		open = s.defaultCAdvisorOpen
	}
	rc, err := open(ctx, node)
	if err != nil {
		return nil, err
	}
	defer func() { _ = rc.Close() }()
	body, err := io.ReadAll(rc)
	if err != nil {
		return nil, fmt.Errorf("read cadvisor body: %w", err)
	}
	return body, nil
}

// defaultCAdvisorOpen issues a kubelet proxy GET via the typed clientset.
// The apiserver enforces nodes/proxy RBAC; the kubelet enforces its own
// auth on top.
func (s *Session) defaultCAdvisorOpen(ctx context.Context, node string) (io.ReadCloser, error) {
	req := s.clientset.CoreV1().RESTClient().Get().
		AbsPath("api", "v1", "nodes", node, "proxy", "metrics", "cadvisor")
	rc, err := req.Stream(ctx)
	if err != nil {
		return nil, fmt.Errorf("proxy cadvisor on node %s: %w", node, err)
	}
	return rc, nil
}
