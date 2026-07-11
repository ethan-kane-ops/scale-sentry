package cgroup

import (
	"fmt"
	"io"
	"math"

	dto "github.com/prometheus/client_model/go"
	"github.com/prometheus/common/expfmt"
	"github.com/prometheus/common/model"
)

// ParseCAdvisor builds a [Stat] from a Kubelet cAdvisor `/metrics/cadvisor`
// scrape, filtered to the given pod + container. The kubelet exposes one
// time-series per (pod, container) tuple plus pod-level rollups whose
// container label is empty; the latter are excluded so the report scores
// the workload container instead of an arbitrary mix.
//
// CFS counters map directly:
//
//	container_cpu_cfs_periods_total           -> Stat.NRPeriods
//	container_cpu_cfs_throttled_periods_total -> Stat.NRThrottled
//	container_cpu_cfs_throttled_seconds_total -> Stat.ThrottledUSec
//	container_cpu_usage_seconds_total         -> Stat.UsageUSec
//
// user_/system_ usage are not exposed by cAdvisor and stay at zero.
func ParseCAdvisor(r io.Reader, podName, namespace, container string) (s Stat, err error) {
	if podName == "" || container == "" {
		return Stat{}, fmt.Errorf("pod and container required for cadvisor filter")
	}
	// The text parser panics on some malformed inputs, e.g. a label brace
	// line before any metric name ("#HELP A00 \n{}"), reproduced by fuzzing
	// on prometheus/common v0.69.0 and v0.70.0. The scrape body arrives
	// over the network, so a parser panic must surface as a parse error,
	// not kill the observer.
	defer func() {
		if p := recover(); p != nil {
			s, err = Stat{}, fmt.Errorf("parse cadvisor exposition: parser panic: %v", p)
		}
	}()
	// prometheus/common v0.67 panics inside the text parser the first
	// time it inspects a metric name unless the validation scheme is
	// set; the constructor takes the scheme explicitly. UTF8Validation
	// matches the upstream default for new code and accepts cAdvisor's
	// snake-case metric names.
	parser := expfmt.NewTextParser(model.UTF8Validation)
	families, err := parser.TextToMetricFamilies(r)
	if err != nil {
		return Stat{}, fmt.Errorf("parse cadvisor exposition: %w", err)
	}

	pick := func(name string, into func(uint64)) {
		fam, ok := families[name]
		if !ok {
			return
		}
		for _, m := range fam.GetMetric() {
			if !matches(m.GetLabel(), podName, namespace, container) {
				continue
			}
			into(uint64(math.Round(counterValue(m))))
			return
		}
	}

	pick("container_cpu_cfs_periods_total", func(v uint64) { s.NRPeriods = v })
	pick("container_cpu_cfs_throttled_periods_total", func(v uint64) { s.NRThrottled = v })
	pick("container_cpu_cfs_throttled_seconds_total", func(v uint64) { s.ThrottledUSec = v * 1_000_000 })
	pick("container_cpu_usage_seconds_total", func(v uint64) { s.UsageUSec = v * 1_000_000 })
	return s, nil
}

// matches reports whether the metric's labels select the workload
// container. The pod_uid label would be more precise, but pod+namespace+
// container is unique within a scrape and survives kubelet relabeling.
func matches(labels []*dto.LabelPair, podName, namespace, container string) bool {
	var pod, ns, c string
	for _, l := range labels {
		switch l.GetName() {
		case "pod":
			pod = l.GetValue()
		case "namespace":
			ns = l.GetValue()
		case "container":
			c = l.GetValue()
		}
	}
	if c == "" {
		return false // pod-level rollup, skip
	}
	if pod != podName || c != container {
		return false
	}
	// Namespace label is sometimes omitted on older kubelets; only enforce
	// when both sides supply it.
	if ns != "" && namespace != "" && ns != namespace {
		return false
	}
	return true
}

// counterValue returns the float64 value of a counter or untyped metric.
// cAdvisor exposes the CFS counters as counters; other types yield zero.
func counterValue(m *dto.Metric) float64 {
	if c := m.GetCounter(); c != nil {
		return c.GetValue()
	}
	if u := m.GetUntyped(); u != nil {
		return u.GetValue()
	}
	return 0
}
