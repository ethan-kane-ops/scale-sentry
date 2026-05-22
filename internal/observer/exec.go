package observer

import (
	"bytes"
	"context"
	"fmt"
	"strings"

	corev1 "k8s.io/api/core/v1"
	"k8s.io/client-go/kubernetes/scheme"
	"k8s.io/client-go/tools/remotecommand"

	"github.com/ethan-kane-ops/scale-sentry/internal/analyzer/cgroup"
)

// cgroupStatPath is the cgroup v2 CPU stat file inside the container.
const cgroupStatPath = "/sys/fs/cgroup/cpu.stat"

// scrapeCgroup execs `cat cpu.stat` in the pod's first container and parses
// the result, feeding the before/after CFS-throttle comparison.
func (s *Session) scrapeCgroup(ctx context.Context, pod *corev1.Pod) (cgroup.Stat, error) {
	if pod == nil || len(pod.Spec.Containers) == 0 {
		return cgroup.Stat{}, fmt.Errorf("no pod or container to scrape")
	}
	out, err := s.execInPod(ctx, pod, pod.Spec.Containers[0].Name, []string{"cat", cgroupStatPath})
	if err != nil {
		return cgroup.Stat{}, err
	}
	return cgroup.Parse(strings.NewReader(out))
}

// execInPod runs command in a container via the pods/exec subresource and
// returns its stdout.
func (s *Session) execInPod(ctx context.Context, pod *corev1.Pod, container string, command []string) (string, error) {
	req := s.clientset.CoreV1().RESTClient().Post().
		Resource("pods").
		Name(pod.Name).
		Namespace(pod.Namespace).
		SubResource("exec").
		VersionedParams(&corev1.PodExecOptions{
			Container: container,
			Command:   command,
			Stdout:    true,
			Stderr:    true,
		}, scheme.ParameterCodec)

	exec, err := remotecommand.NewSPDYExecutor(s.restConfig, "POST", req.URL())
	if err != nil {
		return "", fmt.Errorf("new exec: %w", err)
	}
	var stdout, stderr bytes.Buffer
	if err := exec.StreamWithContext(ctx, remotecommand.StreamOptions{
		Stdout: &stdout,
		Stderr: &stderr,
	}); err != nil {
		return "", fmt.Errorf("exec %v: %w (stderr: %s)", command, err, strings.TrimSpace(stderr.String()))
	}
	return stdout.String(), nil
}
