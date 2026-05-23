package cgroup

import (
	"strings"
	"testing"
)

// A trimmed cAdvisor scrape with two pods and pod-level rollups, so the
// filter for "match this exact pod+container" is exercised against
// realistic noise. Values are chosen as round multiples so the conversion
// to throttled_usec is easy to assert.
const cadvisorSample = `# HELP container_cpu_cfs_periods_total Number of elapsed enforcement period intervals.
# TYPE container_cpu_cfs_periods_total counter
container_cpu_cfs_periods_total{container="",namespace="demo",pod="target-abc",pod_uid="11"} 9999 1735000000000
container_cpu_cfs_periods_total{container="hpa-example",namespace="demo",pod="target-abc",pod_uid="11"} 1000 1735000000000
container_cpu_cfs_periods_total{container="hpa-example",namespace="demo",pod="other-pod",pod_uid="22"} 7777 1735000000000
# HELP container_cpu_cfs_throttled_periods_total Number of throttled period intervals.
# TYPE container_cpu_cfs_throttled_periods_total counter
container_cpu_cfs_throttled_periods_total{container="hpa-example",namespace="demo",pod="target-abc",pod_uid="11"} 250 1735000000000
container_cpu_cfs_throttled_periods_total{container="hpa-example",namespace="demo",pod="other-pod",pod_uid="22"} 0 1735000000000
# HELP container_cpu_cfs_throttled_seconds_total Total time duration the container has been throttled.
# TYPE container_cpu_cfs_throttled_seconds_total counter
container_cpu_cfs_throttled_seconds_total{container="hpa-example",namespace="demo",pod="target-abc",pod_uid="11"} 3 1735000000000
# HELP container_cpu_usage_seconds_total Cumulative cpu time consumed.
# TYPE container_cpu_usage_seconds_total counter
container_cpu_usage_seconds_total{container="hpa-example",namespace="demo",pod="target-abc",pod_uid="11"} 42 1735000000000
`

func TestParseCAdvisor_MatchesTargetContainer(t *testing.T) {
	got, err := ParseCAdvisor(strings.NewReader(cadvisorSample), "target-abc", "demo", "hpa-example")
	if err != nil {
		t.Fatalf("ParseCAdvisor: %v", err)
	}
	if got.NRPeriods != 1000 {
		t.Errorf("NRPeriods = %d, want 1000 (must skip the pod-level rollup at 9999)", got.NRPeriods)
	}
	if got.NRThrottled != 250 {
		t.Errorf("NRThrottled = %d, want 250", got.NRThrottled)
	}
	if got.ThrottledUSec != 3_000_000 {
		t.Errorf("ThrottledUSec = %d, want 3_000_000 (3s)", got.ThrottledUSec)
	}
	if got.UsageUSec != 42_000_000 {
		t.Errorf("UsageUSec = %d, want 42_000_000 (42s)", got.UsageUSec)
	}
}

func TestParseCAdvisor_IgnoresOtherPods(t *testing.T) {
	// Filtering for a pod that has no container-level series returns zero,
	// not the other-pod's counters.
	got, err := ParseCAdvisor(strings.NewReader(cadvisorSample), "missing-pod", "demo", "hpa-example")
	if err != nil {
		t.Fatalf("ParseCAdvisor: %v", err)
	}
	if got != (Stat{}) {
		t.Errorf("got %+v, want zero Stat for missing pod", got)
	}
}

func TestParseCAdvisor_RequiresPodAndContainer(t *testing.T) {
	if _, err := ParseCAdvisor(strings.NewReader(""), "", "demo", "x"); err == nil {
		t.Error("empty pod should return error")
	}
	if _, err := ParseCAdvisor(strings.NewReader(""), "p", "demo", ""); err == nil {
		t.Error("empty container should return error")
	}
}
