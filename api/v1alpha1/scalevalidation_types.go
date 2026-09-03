package v1alpha1

import (
	metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"
)

// CrossVersionObjectReference contains enough information to identify the
// target resource being validated.
type CrossVersionObjectReference struct {
	// API version of the referent.
	APIVersion string `json:"apiVersion"`
	// Kind of the referent (e.g. Deployment).
	Kind string `json:"kind"`
	// Name of the referent.
	Name string `json:"name"`
}

// TargetConfig describes the HTTP endpoint and network path to test.
type TargetConfig struct {
	// Mode determines how the load test target is resolved.
	// +kubebuilder:validation:Enum=ServiceDefault;AutoDiscoverProbe;CustomPath
	Mode string `json:"mode"`

	// CustomPath is the explicit HTTP path to target. Used when mode is CustomPath.
	// +optional
	CustomPath string `json:"customPath,omitempty"`

	// Port is the target port number.
	Port int32 `json:"port"`

	// NetworkPath determines the routing pathway for load traffic.
	// ClusterIP sends traffic directly to the Service inside the cluster.
	// Ingress sends traffic through a classic Ingress controller (legacy
	// path, prefer Gateway for new deployments). Gateway sends traffic
	// through a Gateway API edge (Envoy Gateway and friends). Only one
	// pathway runs per validation to isolate variables.
	// +kubebuilder:validation:Enum=ClusterIP;Ingress;Gateway
	// +kubebuilder:default=ClusterIP
	NetworkPath string `json:"networkPath"`

	// Host overrides the URL host the loadgen Job hits. Empty (default)
	// resolves to "<targetRef.name>.<namespace>.svc.cluster.local",
	// which is correct for ClusterIP runs against a Service that shares
	// the workload name. Set this to point load through an edge: e.g.
	// the Envoy Gateway address for a Gateway run, or the Ingress LB
	// hostname for the legacy Ingress path.
	// +optional
	Host string `json:"host,omitempty"`

	// Protocol selects the wire protocol the loadgen speaks to the
	// target. HTTP1 uses fasthttp (default, backwards-compatible).
	// HTTP2 uses net/http + http2.Transport: ALPN-negotiated h2 for
	// https URLs, prior-knowledge h2c for http URLs. GRPC uses grpc-go
	// to invoke the standard grpc.health.v1.Health/Check probe; combine
	// with the optional GRPC block to scope the probe to a specific
	// upstream service.
	// +kubebuilder:validation:Enum=HTTP1;HTTP2;GRPC
	// +kubebuilder:default=HTTP1
	// +optional
	Protocol string `json:"protocol,omitempty"`

	// GRPC carries gRPC-specific knobs. Only consulted when Protocol=GRPC;
	// ignored otherwise. Empty (or unset) means probe overall server
	// health on the resolved Service host:port.
	// +optional
	GRPC *GRPCConfig `json:"grpc,omitempty"`

	// TLS configures HTTPS verification for the loadgen client. Only
	// applies when the resolved target URL uses the https scheme.
	// +optional
	TLS *TLSConfig `json:"tls,omitempty"`
}

// GRPCConfig carries gRPC-specific knobs for the loadgen Health/Check
// probe. Scoped intentionally narrow: the goal is to exercise h2 framing,
// gRPC trailers, and server-side request handling at the rate the load
// profile dictates, not to drive arbitrary RPC methods. Reflection-based
// method discovery and unary-RPC invocation against arbitrary user
// services are deferred to a future ticket.
type GRPCConfig struct {
	// Service is the upstream service name passed to the Health/Check
	// probe (grpc.health.v1.HealthCheckRequest.service). Empty probes
	// overall server health. Use a non-empty value when the target
	// publishes multiple per-service health entries.
	// +optional
	Service string `json:"service,omitempty"`
}

// TLSConfig configures TLS verification for the loadgen client.
// InsecureSkipVerify and CABundle are mutually exclusive; setting both is
// rejected by CRD validation.
// +kubebuilder:validation:XValidation:rule="!(self.insecureSkipVerify == true && has(self.caBundle))",message="insecureSkipVerify and caBundle are mutually exclusive"
type TLSConfig struct {
	// InsecureSkipVerify disables certificate verification entirely.
	// Use only for dev / CI clusters; production runs should pin a CA.
	// +kubebuilder:default=false
	// +optional
	InsecureSkipVerify bool `json:"insecureSkipVerify,omitempty"`

	// CABundle points to a ConfigMap key containing one or more
	// PEM-encoded certificates trusted by the loadgen client.
	// +optional
	CABundle *CABundleSource `json:"caBundle,omitempty"`
}

// CABundleSource references a PEM bundle living in a Kubernetes object.
// Only ConfigMapRef is supported today; Secret backing is a future ticket.
type CABundleSource struct {
	// ConfigMapRef selects a key in a ConfigMap in the same namespace as
	// the ScaleValidation.
	ConfigMapRef ConfigMapKeyRef `json:"configMapRef"`
}

// ConfigMapKeyRef identifies a single key in a ConfigMap.
type ConfigMapKeyRef struct {
	// Name of the ConfigMap.
	Name string `json:"name"`
	// Key within the ConfigMap data block holding the PEM bundle.
	Key string `json:"key"`
}

// LoadConfig defines the synthetic traffic profile.
type LoadConfig struct {
	// BaseRPS is the starting requests-per-second before dynamic scaling.
	BaseRPS int32 `json:"baseRps"`

	// ConcurrencyFactor is multiplied by CPU cores to compute target RPS.
	ConcurrencyFactor int32 `json:"concurrencyFactor"`

	// WarmupDuration runs traffic against the target before the
	// measurement window opens. Requests are sent (so TCP/TLS handshakes
	// settle, JIT runs, caches warm) but their latencies and counters
	// are excluded from the SLA verdict. Default 0 (no warmup).
	// +optional
	WarmupDuration *metav1.Duration `json:"warmupDuration,omitempty"`

	// Profile selects the arrival-rate shape for the measurement
	// window. Default Constant (today's behaviour). Poisson is the
	// recommended choice for SLA-accurate verdicts because user
	// traffic is open-loop.
	// +optional
	Profile *LoadProfile `json:"profile,omitempty"`
}

// LoadProfile selects the arrival shape for the measurement window.
// Pattern-specific knobs apply only to their named pattern.
type LoadProfile struct {
	// Pattern is the arrival shape.
	// +kubebuilder:validation:Enum=Constant;Poisson;Ramp;Step;Spike
	// +kubebuilder:default=Constant
	Pattern string `json:"pattern"`

	// EndRPS is the terminal rate for Ramp. Required when Pattern=Ramp.
	// +optional
	EndRPS *int32 `json:"endRps,omitempty"`

	// RampDuration is the wall-clock window over which Ramp interpolates
	// from BaseRPS to EndRPS. Required when Pattern=Ramp.
	// +optional
	RampDuration *metav1.Duration `json:"rampDuration,omitempty"`

	// StepRPS is the rate increment per StepDuration interval. Required
	// when Pattern=Step.
	// +optional
	StepRPS *int32 `json:"stepRps,omitempty"`

	// StepDuration is the wall-clock interval between Step climbs.
	// Required when Pattern=Step.
	// +optional
	StepDuration *metav1.Duration `json:"stepDuration,omitempty"`

	// Spikes is the ordered list of spike windows to stitch into the
	// measurement phase. Required when Pattern=Spike.
	// +optional
	Spikes []SpikeWindow `json:"spikes,omitempty"`
}

// SpikeWindow describes a single rate-elevated slice inside a Spike
// measurement phase. At is measured from the start of the measurement
// phase (post-warmup). Spikes are inserted between Constant base slices
// at BaseRPS, so the resulting phase list is constant-spike-constant-...
type SpikeWindow struct {
	// At is the offset from measurement-phase start at which the spike
	// begins.
	At metav1.Duration `json:"at"`
	// Duration is the wall-clock length of the spike.
	Duration metav1.Duration `json:"duration"`
	// RPS is the rate held during the spike. Must be > BaseRPS or the
	// "spike" would actually be a dip.
	RPS int32 `json:"rps"`
}

// DisruptionConfig controls chaos injection during the validation run.
type DisruptionConfig struct {
	// InjectPodDeletion enables terminating a healthy replica during peak load.
	// +kubebuilder:default=false
	InjectPodDeletion bool `json:"injectPodDeletion"`

	// MinReplicasForChaos is the minimum replica count before chaos is allowed.
	// Prevents disruption from causing total unavailability.
	// +kubebuilder:validation:Minimum=2
	// +kubebuilder:default=2
	MinReplicasForChaos int32 `json:"minReplicasForChaos"`

	// TriggerDelay is the duration to wait after load starts before injecting chaos.
	// +optional
	TriggerDelay *metav1.Duration `json:"triggerDelay,omitempty"`
}

// ScaleValidationSpec defines the desired state of a ScaleValidation run.
type ScaleValidationSpec struct {
	// TargetRef points to the workload to validate (e.g. a Deployment).
	TargetRef CrossVersionObjectReference `json:"targetRef"`

	// SLA is the maximum allowed duration for HPA scale-up and pod readiness.
	SLA metav1.Duration `json:"sla"`

	// Target configures the HTTP endpoint and network routing for load traffic.
	Target TargetConfig `json:"target"`

	// Load defines the synthetic traffic profile parameters.
	Load LoadConfig `json:"load"`

	// Disruption configures optional chaos injection during the validation.
	// +optional
	Disruption *DisruptionConfig `json:"disruption,omitempty"`

	// Schedule is an optional cron expression. When set, the validation
	// re-runs on that schedule instead of running exactly once, and each
	// verdict is appended to status.history so a trend is visible from
	// `kubectl get -o json` alone. Standard five-field cron syntax plus
	// the usual descriptors (@hourly, @daily, @every 1h30m).
	//
	// Runs never overlap: the schedule is evaluated only once a run has
	// reached a terminal phase, so a run that overruns its interval
	// delays the next one rather than racing it.
	// +optional
	Schedule string `json:"schedule,omitempty"`

	// Suspend stops future runs, both scheduled ones and the run a spec
	// edit would otherwise trigger. Setting it is itself a spec edit, so
	// it has to outrank that, or suspending would start the very run it
	// is meant to prevent. A run already in flight is left alone to
	// finish, and the last verdict stays on status, so suspending is safe
	// mid-run and reversible.
	// +optional
	// +kubebuilder:default=false
	Suspend bool `json:"suspend,omitempty"`
}

// DiagnosticAlert represents a single finding from the analysis engine.
type DiagnosticAlert struct {
	// Type categorises the alert (e.g. "ProbeLeakage", "CPUThrottling").
	Type string `json:"type"`
	// Severity is one of Info, Warning, or Critical.
	// +kubebuilder:validation:Enum=Info;Warning;Critical
	Severity string `json:"severity"`
	// Message is a human-readable description of the finding.
	Message string `json:"message"`
	// Recommendation is the suggested remediation.
	// +optional
	Recommendation string `json:"recommendation,omitempty"`
}

// RunHistoryLimit is the maximum number of entries kept in
// ScaleValidationStatus.History. Older entries are dropped oldest-first as
// new runs complete. Fixed rather than spec-configurable so a CR can't grow
// status without bound from user input.
const RunHistoryLimit = 10

// RunSummary is a compact record of one terminal run, kept in
// ScaleValidationStatus.History so trend ("did this get worse over the
// last N releases") is visible from `kubectl get -o json` alone, without
// requiring a Prometheus scrape of scale_sentry_runs_total. Deliberately
// excludes Diagnostics: that field can grow arbitrarily large per run,
// which is fine for a single current-status snapshot but would make a
// History slice's size unbounded.
type RunSummary struct {
	// FinishedAt is when this run reached a terminal phase.
	FinishedAt metav1.Time `json:"finishedAt"`
	// Phase is the terminal phase this run reached (Succeeded or Failed).
	Phase string `json:"phase"`
	// SLAStatus mirrors the top-level status.slaStatus at run completion.
	// +kubebuilder:validation:Enum=Pass;Fail;Unknown
	SLAStatus string `json:"slaStatus,omitempty"`
	// TrafficIntegrity mirrors the top-level status.trafficIntegrity at run completion.
	// +kubebuilder:validation:Enum=Pass;Fail;Unknown
	TrafficIntegrity string `json:"trafficIntegrity,omitempty"`
	// FailureRate mirrors the top-level status.failureRate at run completion.
	FailureRate float64 `json:"failureRate,omitempty"`
}

// ScaleValidationStatus defines the observed state of a ScaleValidation run.
type ScaleValidationStatus struct {
	// Phase represents the current lifecycle state.
	// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Error;Terminating
	Phase string `json:"phase,omitempty"`

	// ScaleUpDuration is the measured time from HPA trigger to all replicas ready.
	// +optional
	ScaleUpDuration *metav1.Duration `json:"scaleUpDuration,omitempty"`

	// SLAStatus indicates whether the scaling met the configured SLA.
	// +kubebuilder:validation:Enum=Pass;Fail;Unknown
	SLAStatus string `json:"slaStatus,omitempty"`

	// TrafficIntegrity indicates whether any requests were dropped.
	// +kubebuilder:validation:Enum=Pass;Fail;Unknown
	TrafficIntegrity string `json:"trafficIntegrity,omitempty"`

	// TotalRequests is the total number of HTTP requests sent during the run.
	TotalRequests int64 `json:"totalRequests,omitempty"`

	// FailedRequests is the number of HTTP requests that returned errors.
	FailedRequests int64 `json:"failedRequests,omitempty"`

	// FailureRate is the ratio of failed to total requests (e.g. 0.0066 = 0.66%).
	FailureRate float64 `json:"failureRate,omitempty"`

	// Diagnostics contains the list of analysis findings.
	// +optional
	Diagnostics []DiagnosticAlert `json:"diagnostics,omitempty"`

	// LastRunTime is the timestamp of the most recent validation execution.
	// +optional
	LastRunTime *metav1.Time `json:"lastRunTime,omitempty"`

	// History holds the most recent terminal runs, newest first, bounded to
	// RunHistoryLimit entries. Lets a single `kubectl get -o json` answer
	// "did this get worse over the last N releases" without a metrics stack.
	// +optional
	// +kubebuilder:validation:MaxItems=10
	History []RunSummary `json:"history,omitempty"`

	// NextRunTime is when the next scheduled run is due. Empty for a
	// one-shot validation, and cleared while spec.suspend is true so
	// `kubectl get` never advertises a run that will not happen.
	// +optional
	NextRunTime *metav1.Time `json:"nextRunTime,omitempty"`

	// ObservedGeneration is the metadata.generation the last terminal
	// result was produced from. When it lags metadata.generation the spec
	// has been edited since, and the controller starts a fresh run rather
	// than leaving a result that describes a spec the object no longer
	// carries.
	// +optional
	ObservedGeneration int64 `json:"observedGeneration,omitempty"`

	// Conditions follow the standard Kubernetes conditions pattern.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="SLA",type=string,JSONPath=`.status.slaStatus`
// +kubebuilder:printcolumn:name="Traffic",type=string,JSONPath=`.status.trafficIntegrity`
// +kubebuilder:printcolumn:name="Schedule",type=string,JSONPath=`.spec.schedule`
// +kubebuilder:printcolumn:name="Next Run",type=date,JSONPath=`.status.nextRunTime`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`

// ScaleValidation is the Schema for the scalevalidations API.
type ScaleValidation struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   ScaleValidationSpec   `json:"spec,omitempty"`
	Status ScaleValidationStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true

// ScaleValidationList contains a list of ScaleValidation.
type ScaleValidationList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []ScaleValidation `json:"items"`
}

func init() {
	SchemeBuilder.Register(&ScaleValidation{}, &ScaleValidationList{})
}
