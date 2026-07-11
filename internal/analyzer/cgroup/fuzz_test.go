package cgroup

import (
	"strings"
	"testing"
)

func FuzzParse(f *testing.F) {
	f.Add("usage_usec 12345678\nuser_usec 100\nsystem_usec 200\nnr_periods 100\nnr_throttled 4\nthrottled_usec 900\n")
	f.Add("nr_periods banana\n")
	f.Add("nr_periods 18446744073709551615\n")
	f.Add("")
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = Parse(strings.NewReader(s))
	})
}

func FuzzParseCAdvisor(f *testing.F) {
	f.Add(cadvisorSample)
	f.Add(`container_cpu_cfs_periods_total{container="c",namespace="ns",pod="p"} NaN`)
	f.Add("# HELP only comments\n")
	f.Fuzz(func(t *testing.T, s string) {
		_, _ = ParseCAdvisor(strings.NewReader(s), "target-abc", "demo", "hpa-example")
	})
}
