// Package observer runs the cluster-side observation for a single
// ScaleValidation run. It is the only scale-sentry library besides the
// controller that holds a Kubernetes client: it polls the target HPA,
// watches EndpointSlices, scrapes cgroup cpu.stat, and collects pod
// conditions, then drives the pure analyzer packages and emits a [Report].
//
// It runs as a native sidecar in the loadgen Job, started before the load
// generator, terminated (SIGTERM) once the load run exits. On SIGTERM it
// finalizes (final cgroup sample, pod conditions, loadgen result file),
// runs every analyzer, and prints the Report as JSON for the controller.
package observer

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strings"
	"sync"
	"time"

	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
	"k8s.io/client-go/dynamic"
	"k8s.io/client-go/kubernetes"
	"k8s.io/client-go/rest"

	v1beta1 "github.com/ethan-kane-ops/scale-sentry/api/v1beta1"
	"github.com/ethan-kane-ops/scale-sentry/internal/analyzer/cgroup"
	"github.com/ethan-kane-ops/scale-sentry/internal/analyzer/dns"
	"github.com/ethan-kane-ops/scale-sentry/internal/analyzer/drain"
	"github.com/ethan-kane-ops/scale-sentry/internal/analyzer/hpa"
	"github.com/ethan-kane-ops/scale-sentry/internal/analyzer/leakage"
	"github.com/ethan-kane-ops/scale-sentry/internal/analyzer/pdb"
	"github.com/ethan-kane-ops/scale-sentry/internal/analyzer/probelag"
	"github.com/ethan-kane-ops/scale-sentry/internal/loadgen"
)

// Verdict values for SLAStatus / TrafficIntegrity. Aliases of the API
// types rather than a parallel set of strings: these end up verbatim on
// ScaleValidation.status, so a second definition could drift from the CRD
// enum without anything noticing.
const (
	VerdictPass    = v1beta1.VerdictPass
	VerdictFail    = v1beta1.VerdictFail
	VerdictUnknown = v1beta1.VerdictUnknown
)

// defaultPollInterval is how often the HPA is sampled during the run.
const defaultPollInterval = 5 * time.Second

// trafficFailThreshold is the failed-request ratio above which traffic
// integrity is marked Fail. A validation run is expected to be near-clean;
// >1% sustained failures is a real resilience defect.
const trafficFailThreshold = 0.01

// finalizeTimeout bounds the post-SIGTERM finalization (final cgroup exec,
// pod list, analysis) so the observer exits within the Job pod's
// termination grace period.
const finalizeTimeout = 15 * time.Second

// Config holds the observer's run parameters, supplied by the controller
// as flags on the sidecar container.
type Config struct {
	// TargetName / Namespace identify the workload under test.
	TargetName string
	Namespace  string
	// TargetKind is the workload's Kind, used to match the HPA whose
	// scaleTargetRef points at it. TargetGroup / TargetVersion /
	// TargetResource are the same workload as a GroupVersionResource, so
	// the observer can read its scale subresource without running its own
	// discovery: the controller already has a RESTMapper and resolves the
	// mapping there. All four default to apps/v1 Deployment.
	TargetKind     string
	TargetGroup    string
	TargetVersion  string
	TargetResource string
	// ServiceName is the Service whose EndpointSlices are watched. Empty
	// falls back to TargetName (the ENG-35 same-name assumption).
	ServiceName string
	// SLA bounds HPA reaction + settle time.
	SLA time.Duration
	// PollInterval is the HPA sampling cadence; zero uses defaultPollInterval.
	PollInterval time.Duration
	// ResultFile is the shared-volume path the loadgen container writes its
	// JSON Result to. Empty skips traffic correlation.
	ResultFile string
}

// Report is the observer's output, printed as JSON for the controller to
// fold into ScaleValidationStatus.
type Report struct {
	Diagnostics      []v1beta1.DiagnosticAlert `json:"diagnostics"`
	ScaleUpDuration  *metav1.Duration          `json:"scaleUpDuration,omitempty"`
	SLAStatus        v1beta1.Verdict           `json:"slaStatus"`
	TrafficIntegrity v1beta1.Verdict           `json:"trafficIntegrity"`
	TotalRequests    int64                     `json:"totalRequests"`
	FailedRequests   int64                     `json:"failedRequests"`
	FailureRate      float64                   `json:"failureRate"`
}

// Session carries the observer's clients and accumulated state for one run.
type Session struct {
	cfg        Config
	clientset  kubernetes.Interface
	dyn        dynamic.Interface
	restConfig *rest.Config

	// cadvisorOpen overrides the kubelet cAdvisor scrape opener in tests
	// so the cgroup pipeline can be exercised without a live kubelet.
	// Production leaves it nil and falls through to the typed clientset.
	cadvisorOpen cadvisorReader

	mu     sync.Mutex
	events []leakage.EndpointEvent
}

// NewSession builds a Session from an in-cluster (or kubeconfig) rest config.
func NewSession(cfg Config, restConfig *rest.Config) (*Session, error) {
	cs, err := kubernetes.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("build clientset: %w", err)
	}
	dyn, err := dynamic.NewForConfig(restConfig)
	if err != nil {
		return nil, fmt.Errorf("build dynamic client: %w", err)
	}
	if cfg.PollInterval <= 0 {
		cfg.PollInterval = defaultPollInterval
	}
	if cfg.ServiceName == "" {
		cfg.ServiceName = cfg.TargetName
	}
	cfg.applyTargetDefaults()
	return &Session{cfg: cfg, clientset: cs, dyn: dyn, restConfig: restConfig}, nil
}

// Run observes the cluster until ctx is cancelled (SIGTERM on load-run
// exit), then finalizes and returns the Report. Run never returns an error:
// any data source it cannot reach degrades to a missing diagnostic and an
// Unknown verdict so the controller always gets a result.
func (s *Session) Run(ctx context.Context) Report {
	start := time.Now()

	target, err := s.resolveTarget(ctx)
	if err != nil {
		warn("resolve target: %v", err)
		return s.report(nil, nil, s.loadResult())
	}

	var watcher *hpa.Watcher
	if target.hpa != nil {
		watcher = hpa.New(snapshotHPA(target.hpa, start), s.cfg.SLA)
	}

	cgBefore, err := s.scrapeCgroup(ctx, target.samplePod)
	if err != nil {
		warn("cgroup sample (before): %v", err)
	}

	watchCtx, stopWatch := context.WithCancel(ctx)
	defer stopWatch()
	go s.watchEndpoints(watchCtx)

	s.pollHPA(ctx, watcher, target)
	end := time.Now()
	stopWatch()

	// Finalization runs on a fresh context, ctx is already cancelled.
	fin, cancel := context.WithTimeout(context.Background(), finalizeTimeout)
	defer cancel()

	var cgReport *cgroup.Report
	if cgAfter, err := s.scrapeCgroup(fin, target.samplePod); err != nil {
		warn("cgroup sample (after): %v", err)
	} else {
		r := cgroup.Compare(cgBefore, cgAfter, end.Sub(start))
		cgReport = &r
	}

	probeReport := s.collectProbeLag(fin, target, start)
	res := s.collectResilience(fin, target)

	var hpaReport *hpa.Report
	if watcher != nil {
		r := watcher.Report()
		hpaReport = &r
	}

	return s.report(hpaReport, &observed{cgroup: cgReport, probelag: probeReport, resilience: res}, s.loadResult())
}

// observed bundles the optional analyzer outputs gathered during the run.
type observed struct {
	cgroup   *cgroup.Report
	probelag *probelag.Report
	// resilience holds the configuration audits (DNS, PDB). Unlike the
	// other analyzers these read the workload's declared posture rather
	// than samples taken during the run, so each half is independently
	// nil when its lookup was unavailable.
	resilience resilience
}

// resilience bundles the configuration-audit outputs. A nil field means
// the audit could not run (missing pod, missing RBAC), not that it passed.
type resilience struct {
	dns *dns.Report
	pdb *pdb.Report
}

// report assembles the final Report from the analyzer outputs, the loadgen
// result, and the accumulated endpoint events.
func (s *Session) report(hpaReport *hpa.Report, obs *observed, load loadResult) Report {
	rep := Report{
		SLAStatus:        VerdictUnknown,
		TrafficIntegrity: VerdictUnknown,
	}

	var diags []v1beta1.DiagnosticAlert
	if hpaReport != nil {
		diags = append(diags, hpaReport.Diagnostics()...)
		rep.SLAStatus = slaVerdict(*hpaReport)
		if hpaReport.Settled {
			d := metav1.Duration{Duration: hpaReport.SettleLatency}
			rep.ScaleUpDuration = &d
		}
	}
	if obs != nil {
		if obs.cgroup != nil {
			diags = append(diags, obs.cgroup.Diagnostics()...)
		}
		if obs.probelag != nil {
			diags = append(diags, obs.probelag.Diagnostics()...)
		}
		if obs.resilience.dns != nil {
			diags = append(diags, obs.resilience.dns.Diagnostics()...)
		}
		if obs.resilience.pdb != nil {
			diags = append(diags, obs.resilience.pdb.Diagnostics()...)
		}
	}

	events := s.snapshotEvents()
	if load.ok {
		rep.TotalRequests = load.result.Sent
		rep.FailedRequests = load.result.Failed
		rep.FailureRate = load.result.FailureRate()
		rep.TrafficIntegrity = trafficVerdict(load.result)

		errs := toLeakageSamples(load.result.ErrorSamples)
		correlationDiags := leakage.Correlate(events, errs, 0).Diagnostics()
		correlationDiags = append(correlationDiags, drain.Correlate(events, errs, 0).Diagnostics()...)
		if len(correlationDiags) > 0 {
			// Endpoint event timestamps come from the informer's local
			// receive time, not from the kubelet/apiserver decision time
			// so they trail reality by anywhere from a few ms (warm
			// watch) to several hundred (cold watch / busy apiserver).
			// Flag whenever a correlation analyzer fires so the operator
			// reads the leakage / drain counts as an approximation
			// instead of a precise audit.
			correlationDiags = append(correlationDiags, v1beta1.DiagnosticAlert{
				Type:           "MetricsLikelySkewed",
				Severity:       "Info",
				Message:        "endpoint event timestamps reflect informer-watch receive time, not kubelet/apiserver decision time; leakage and drain counts can be off by tens to hundreds of milliseconds",
				Recommendation: "treat the leakage / drain counts as directional; cross-check with kubelet logs or audit events when the failure count looks borderline",
			})
		}
		diags = append(diags, correlationDiags...)
	}

	rep.Diagnostics = diags
	return rep
}

// loadResult is the parsed loadgen result file plus a presence flag.
type loadResult struct {
	result *loadgen.Result
	ok     bool
}

// loadResult reads and parses the loadgen result file. A missing or
// unreadable file yields ok=false (traffic verdict becomes Unknown).
func (s *Session) loadResult() loadResult {
	if s.cfg.ResultFile == "" {
		return loadResult{}
	}
	raw, err := os.ReadFile(s.cfg.ResultFile)
	if err != nil {
		warn("read loadgen result %s: %v", s.cfg.ResultFile, err)
		return loadResult{}
	}
	var r loadgen.Result
	if err := json.Unmarshal(raw, &r); err != nil {
		warn("decode loadgen result: %v", err)
		return loadResult{}
	}
	return loadResult{result: &r, ok: true}
}

// snapshotEvents returns a copy of the accumulated endpoint events.
func (s *Session) snapshotEvents() []leakage.EndpointEvent {
	s.mu.Lock()
	defer s.mu.Unlock()
	out := make([]leakage.EndpointEvent, len(s.events))
	copy(out, s.events)
	return out
}

// addEvents appends endpoint events under the session lock.
func (s *Session) addEvents(evs []leakage.EndpointEvent) {
	if len(evs) == 0 {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	s.events = append(s.events, evs...)
}

// slaVerdict maps an HPA report to a Pass/Fail verdict.
func slaVerdict(r hpa.Report) v1beta1.Verdict {
	if r.SLABreached {
		return VerdictFail
	}
	return VerdictPass
}

// trafficVerdict maps a loadgen result to a Pass/Fail verdict.
func trafficVerdict(r *loadgen.Result) v1beta1.Verdict {
	if r == nil {
		return VerdictUnknown
	}
	if r.FailureRate() > trafficFailThreshold {
		return VerdictFail
	}
	return VerdictPass
}

// toLeakageSamples converts loadgen error samples into the leakage/drain
// analyzers' input type.
func toLeakageSamples(in []loadgen.ErrorSample) []leakage.ErrorSample {
	out := make([]leakage.ErrorSample, len(in))
	for i, s := range in {
		out[i] = leakage.ErrorSample{At: s.At, Category: string(s.Category), Status: s.Status}
	}
	return out
}

// warn logs a non-fatal observer problem to stderr.
func warn(format string, args ...any) {
	fmt.Fprintf(os.Stderr, "observer: "+format+"\n", args...)
}

// ReportLogPrefix marks the single stdout line carrying the JSON Report.
// The controller scans the observer container's logs for this prefix to
// extract the result, immune to interleaved stderr diagnostics.
const ReportLogPrefix = "SCALE_SENTRY_REPORT "

// MarshalReportLine renders r as the single marker-prefixed line the
// observer prints to stdout on exit.
func MarshalReportLine(r Report) (string, error) {
	b, err := json.Marshal(r)
	if err != nil {
		return "", fmt.Errorf("marshal report: %w", err)
	}
	return ReportLogPrefix + string(b), nil
}

// ParseReportLog extracts the Report from a container's log output by
// finding the marker line. Returns an error if no marker line is present.
func ParseReportLog(log []byte) (Report, error) {
	for _, line := range strings.Split(string(log), "\n") {
		rest, ok := strings.CutPrefix(strings.TrimSpace(line), ReportLogPrefix)
		if !ok {
			continue
		}
		var r Report
		if err := json.Unmarshal([]byte(rest), &r); err != nil {
			return Report{}, fmt.Errorf("decode observer report: %w", err)
		}
		return r, nil
	}
	return Report{}, fmt.Errorf("no observer report line found in log")
}
