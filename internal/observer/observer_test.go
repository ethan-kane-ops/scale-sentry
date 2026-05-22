package observer

import (
	"testing"
	"time"

	autoscalingv2 "k8s.io/api/autoscaling/v2"
	corev1 "k8s.io/api/core/v1"
	discoveryv1 "k8s.io/api/discovery/v1"
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

	"github.com/ethan-kane-ops/scale-sentry/internal/analyzer/hpa"
	"github.com/ethan-kane-ops/scale-sentry/internal/analyzer/leakage"
	"github.com/ethan-kane-ops/scale-sentry/internal/loadgen"
)

func boolPtr(b bool) *bool { return &b }

func endpoint(ip string, ready bool) discoveryv1.Endpoint {
	return discoveryv1.Endpoint{
		Addresses:  []string{ip},
		Conditions: discoveryv1.EndpointConditions{Ready: boolPtr(ready)},
	}
}

// eventsByIP indexes a slice of endpoint events by pod IP. The tracker
// derives events from map iteration, so order is not asserted.
func eventsByIP(evs []leakage.EndpointEvent) map[string]leakage.EventKind {
	out := map[string]leakage.EventKind{}
	for _, e := range evs {
		out[e.PodIP] = e.Kind
	}
	return out
}

func TestEndpointTracker(t *testing.T) {
	tr := newEndpointTracker()
	at := time.Now()

	// First sight of a ready pod -> one Ready event.
	got := eventsByIP(tr.apply("s1", []discoveryv1.Endpoint{endpoint("10.0.0.1", true)}, false, at))
	if len(got) != 1 || got["10.0.0.1"] != leakage.EndpointReady {
		t.Fatalf("first apply = %v, want 10.0.0.1 Ready", got)
	}

	// Re-applying the same state emits nothing.
	if evs := tr.apply("s1", []discoveryv1.Endpoint{endpoint("10.0.0.1", true)}, false, at); len(evs) != 0 {
		t.Fatalf("no-change apply emitted %v", evs)
	}

	// A second ready pod appears -> one Ready event for it only.
	got = eventsByIP(tr.apply("s1", []discoveryv1.Endpoint{
		endpoint("10.0.0.1", true), endpoint("10.0.0.2", true),
	}, false, at))
	if len(got) != 1 || got["10.0.0.2"] != leakage.EndpointReady {
		t.Fatalf("second pod apply = %v, want 10.0.0.2 Ready", got)
	}

	// Pod .2 drops out of the slice -> Removed.
	got = eventsByIP(tr.apply("s1", []discoveryv1.Endpoint{endpoint("10.0.0.1", true)}, false, at))
	if len(got) != 1 || got["10.0.0.2"] != leakage.EndpointRemoved {
		t.Fatalf("pod-gone apply = %v, want 10.0.0.2 Removed", got)
	}

	// A pod going not-ready also counts as Removed.
	got = eventsByIP(tr.apply("s1", []discoveryv1.Endpoint{endpoint("10.0.0.1", false)}, false, at))
	if len(got) != 1 || got["10.0.0.1"] != leakage.EndpointRemoved {
		t.Fatalf("not-ready apply = %v, want 10.0.0.1 Removed", got)
	}
}

func TestEndpointTracker_MultiSlice(t *testing.T) {
	tr := newEndpointTracker()
	at := time.Now()

	tr.apply("s1", []discoveryv1.Endpoint{endpoint("10.0.0.1", true)}, false, at)
	got := eventsByIP(tr.apply("s2", []discoveryv1.Endpoint{endpoint("10.0.0.2", true)}, false, at))
	if len(got) != 1 || got["10.0.0.2"] != leakage.EndpointReady {
		t.Fatalf("second slice = %v, want 10.0.0.2 Ready", got)
	}

	// Deleting slice s1 removes only its endpoints; s2's pod stays.
	got = eventsByIP(tr.apply("s1", nil, true, at))
	if len(got) != 1 || got["10.0.0.1"] != leakage.EndpointRemoved {
		t.Fatalf("slice delete = %v, want only 10.0.0.1 Removed", got)
	}
}

func TestSLAVerdict(t *testing.T) {
	if v := slaVerdict(hpa.Report{SLABreached: true}); v != VerdictFail {
		t.Errorf("breached -> %q, want Fail", v)
	}
	if v := slaVerdict(hpa.Report{SLABreached: false}); v != VerdictPass {
		t.Errorf("not breached -> %q, want Pass", v)
	}
}

func TestTrafficVerdict(t *testing.T) {
	tests := []struct {
		name   string
		result *loadgen.Result
		want   string
	}{
		{"no result", nil, VerdictUnknown},
		{"clean run", &loadgen.Result{Sent: 1000, Failed: 0}, VerdictPass},
		{"under threshold", &loadgen.Result{Sent: 1000, Failed: 5}, VerdictPass},
		{"over threshold", &loadgen.Result{Sent: 1000, Failed: 50}, VerdictFail},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if v := trafficVerdict(tt.result); v != tt.want {
				t.Errorf("trafficVerdict = %q, want %q", v, tt.want)
			}
		})
	}
}

func TestSnapshotHPA(t *testing.T) {
	min := int32(2)
	h := &autoscalingv2.HorizontalPodAutoscaler{
		Spec:   autoscalingv2.HorizontalPodAutoscalerSpec{MinReplicas: &min, MaxReplicas: 10},
		Status: autoscalingv2.HorizontalPodAutoscalerStatus{CurrentReplicas: 3, DesiredReplicas: 7},
	}
	at := time.Now()
	snap := snapshotHPA(h, at)
	if snap.At != at || snap.CurrentReplicas != 3 || snap.DesiredReplicas != 7 ||
		snap.MinReplicas != 2 || snap.MaxReplicas != 10 {
		t.Errorf("snapshotHPA = %+v", snap)
	}

	// Nil MinReplicas resolves to 0, not a panic.
	h.Spec.MinReplicas = nil
	if snap := snapshotHPA(h, at); snap.MinReplicas != 0 {
		t.Errorf("nil MinReplicas -> %d, want 0", snap.MinReplicas)
	}
}

func TestToLeakageSamples(t *testing.T) {
	at := time.Now()
	in := []loadgen.ErrorSample{
		{At: at, Category: loadgen.ErrServer, Status: 503},
		{At: at, Category: loadgen.ErrTimeout, Status: 0},
	}
	out := toLeakageSamples(in)
	if len(out) != 2 {
		t.Fatalf("len = %d, want 2", len(out))
	}
	if out[0].Category != "Server5xx" || out[0].Status != 503 {
		t.Errorf("out[0] = %+v", out[0])
	}
	if out[1].Category != "Timeout" || out[1].Status != 0 {
		t.Errorf("out[1] = %+v", out[1])
	}
}

func TestNewestPod(t *testing.T) {
	mk := func(name string, ageMin int) corev1.Pod {
		return corev1.Pod{ObjectMeta: metav1.ObjectMeta{
			Name:              name,
			CreationTimestamp: metav1.NewTime(time.Now().Add(-time.Duration(ageMin) * time.Minute)),
		}}
	}
	pods := []corev1.Pod{mk("old", 30), mk("newest", 1), mk("mid", 10)}
	if got := newestPod(pods); got.Name != "newest" {
		t.Errorf("newestPod = %s, want newest", got.Name)
	}
}

func TestParseReportLog(t *testing.T) {
	rep := Report{SLAStatus: VerdictPass, TrafficIntegrity: VerdictFail, TotalRequests: 500}
	line, err := MarshalReportLine(rep)
	if err != nil {
		t.Fatalf("MarshalReportLine: %v", err)
	}

	// The marker line is surrounded by interleaved stderr diagnostics.
	log := []byte("observer: watching demo/app\nobserver: get HPA: timeout\n" + line + "\n")
	got, err := ParseReportLog(log)
	if err != nil {
		t.Fatalf("ParseReportLog: %v", err)
	}
	if got.SLAStatus != VerdictPass || got.TrafficIntegrity != VerdictFail || got.TotalRequests != 500 {
		t.Errorf("round-trip mismatch: %+v", got)
	}

	if _, err := ParseReportLog([]byte("just some logs\nno marker\n")); err == nil {
		t.Error("expected error when the log has no marker line")
	}
}

func TestReadinessPeriodSeconds(t *testing.T) {
	withProbe := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{
		ReadinessProbe: &corev1.Probe{PeriodSeconds: 15},
	}}}}
	if got := readinessPeriodSeconds(withProbe); got != 15 {
		t.Errorf("with probe = %d, want 15", got)
	}

	noProbe := &corev1.Pod{Spec: corev1.PodSpec{Containers: []corev1.Container{{}}}}
	if got := readinessPeriodSeconds(noProbe); got != 0 {
		t.Errorf("no probe = %d, want 0", got)
	}
}
