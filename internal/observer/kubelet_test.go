package observer

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"

	corev1 "k8s.io/api/core/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// stubOpener returns a fixed body for any node, simulating a kubelet
// cAdvisor scrape so the observer's scrape pipeline can be exercised
// without a live kubelet.
func stubOpener(body string) cadvisorReader {
	return func(_ context.Context, _ string) (io.ReadCloser, error) {
		return io.NopCloser(strings.NewReader(body)), nil
	}
}

const cadvisorScrapeForPod = `# TYPE container_cpu_cfs_periods_total counter
container_cpu_cfs_periods_total{container="web",namespace="demo",pod="target",pod_uid="1"} 500 0
# TYPE container_cpu_cfs_throttled_periods_total counter
container_cpu_cfs_throttled_periods_total{container="web",namespace="demo",pod="target",pod_uid="1"} 25 0
`

func TestScrapeCgroup_HappyPath(t *testing.T) {
	s := &Session{
		cfg:          Config{Namespace: "demo"},
		cadvisorOpen: stubOpener(cadvisorScrapeForPod),
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "target", Namespace: "demo"},
		Spec: corev1.PodSpec{
			NodeName:   "node-a",
			Containers: []corev1.Container{{Name: "web"}},
		},
	}
	got, err := s.scrapeCgroup(context.Background(), pod)
	if err != nil {
		t.Fatalf("scrapeCgroup: %v", err)
	}
	if got.NRPeriods != 500 || got.NRThrottled != 25 {
		t.Errorf("scrapeCgroup = %+v, want NRPeriods=500 NRThrottled=25", got)
	}
}

func TestScrapeCgroup_RejectsUnscheduledPod(t *testing.T) {
	s := &Session{cadvisorOpen: stubOpener("")}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "pending", Namespace: "demo"},
		Spec:       corev1.PodSpec{Containers: []corev1.Container{{Name: "web"}}},
	}
	if _, err := s.scrapeCgroup(context.Background(), pod); err == nil {
		t.Error("expected error when pod has no nodeName")
	}
}

func TestScrapeCgroup_OpenerError(t *testing.T) {
	want := errors.New("proxy refused")
	s := &Session{
		cadvisorOpen: func(_ context.Context, _ string) (io.ReadCloser, error) { return nil, want },
	}
	pod := &corev1.Pod{
		ObjectMeta: metav1.ObjectMeta{Name: "p", Namespace: "demo"},
		Spec: corev1.PodSpec{
			NodeName:   "node-a",
			Containers: []corev1.Container{{Name: "web"}},
		},
	}
	if _, err := s.scrapeCgroup(context.Background(), pod); !errors.Is(err, want) {
		t.Errorf("err = %v, want %v wrapped", err, want)
	}
}
