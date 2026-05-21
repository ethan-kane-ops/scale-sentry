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
}

// LoadConfig defines the synthetic traffic profile.
type LoadConfig struct {
	// BaseRPS is the starting requests-per-second before dynamic scaling.
	BaseRPS int32 `json:"baseRps"`

	// ConcurrencyFactor is multiplied by CPU cores to compute target RPS.
	ConcurrencyFactor int32 `json:"concurrencyFactor"`
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
	// +kubebuilder:validation:Enum=Pending;Running;Succeeded;Failed;Error
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
