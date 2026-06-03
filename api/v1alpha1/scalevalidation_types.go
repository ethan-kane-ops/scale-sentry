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
	// Ingress sends traffic through the edge Ingress controller.
	// Only one pathway runs per validation to isolate variables.
	// +kubebuilder:validation:Enum=ClusterIP;Ingress
	// +kubebuilder:default=ClusterIP
	NetworkPath string `json:"networkPath"`

	// Protocol selects the HTTP wire protocol the loadgen speaks to
	// the target. HTTP1 uses fasthttp (default, backwards-compatible).
	// HTTP2 uses net/http + http2.Transport: ALPN-negotiated h2 for
	// https URLs, prior-knowledge h2c for http URLs. Use HTTP2 when
	// the target is a gRPC service, an envoy-fronted backend, or any
	// workload whose scaling characteristics depend on stream
	// multiplexing rather than per-request connection cost.
	// +kubebuilder:validation:Enum=HTTP1;HTTP2
	// +kubebuilder:default=HTTP1
	// +optional
	Protocol string `json:"protocol,omitempty"`

	// TLS configures HTTPS verification for the loadgen client. Only
	// applies when the resolved target URL uses the https scheme.
	// +optional
	TLS *TLSConfig `json:"tls,omitempty"`
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

	// Conditions follow the standard Kubernetes conditions pattern.
	// +optional
	Conditions []metav1.Condition `json:"conditions,omitempty"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="SLA",type=string,JSONPath=`.status.slaStatus`
// +kubebuilder:printcolumn:name="Traffic",type=string,JSONPath=`.status.trafficIntegrity`
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
