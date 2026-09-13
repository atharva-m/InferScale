package v1alpha1

import metav1 "k8s.io/apimachinery/pkg/apis/meta/v1"

// +kubebuilder:validation:Enum=auto;vllm;tensorrt-llm
type RuntimeBackend string

const (
	RuntimeBackendAuto     RuntimeBackend = "auto"
	RuntimeBackendVLLM     RuntimeBackend = "vllm"
	RuntimeBackendTensorRT RuntimeBackend = "tensorrt-llm"
)

// +kubebuilder:validation:Enum=round-robin;load-aware;prefix-aware
type RoutingPolicy string

const (
	RoutingPolicyRoundRobin  RoutingPolicy = "round-robin"
	RoutingPolicyLoadAware   RoutingPolicy = "load-aware"
	RoutingPolicyPrefixAware RoutingPolicy = "prefix-aware"
)

type DeploymentPhase string

const (
	DeploymentPhasePending     DeploymentPhase = "Pending"
	DeploymentPhasePrefetching DeploymentPhase = "Prefetching"
	DeploymentPhaseDeploying   DeploymentPhase = "Deploying"
	DeploymentPhaseReady       DeploymentPhase = "Ready"
	DeploymentPhaseUpdating    DeploymentPhase = "Updating"
	DeploymentPhaseDegraded    DeploymentPhase = "Degraded"
	DeploymentPhaseFailed      DeploymentPhase = "Failed"
	DeploymentPhaseDeleting    DeploymentPhase = "Deleting"
)

// +kubebuilder:validation:XValidation:rule="self.accelerator.count == self.runtime.tensorParallelism",message="accelerator count must equal tensor parallelism in v1"
// +kubebuilder:validation:XValidation:rule="self.scaling.minReplicas <= self.scaling.maxReplicas",message="minReplicas must not exceed maxReplicas"
// +kubebuilder:validation:XValidation:rule="!(self.runtime.precision == 'fp8' && self.runtime.quantization == 'awq')",message="fp8 precision cannot be combined with awq quantization"
// +kubebuilder:validation:XValidation:rule="self.routing.policy != 'prefix-aware' || self.runtime.prefixCaching.enabled",message="prefix-aware routing requires prefix caching"
// +kubebuilder:validation:XValidation:rule="self.runtime.backend != 'tensorrt-llm' || self.routing.policy != 'prefix-aware'",message="TensorRT-LLM does not support prefix-aware routing in v1"
// +kubebuilder:validation:XValidation:rule="self.runtime.backend != 'tensorrt-llm' || self.runtime.quantization == 'none'",message="TensorRT-LLM supports only quantization=none in v1"
// +kubebuilder:validation:XValidation:rule="self.runtime.backend != 'tensorrt-llm' || self.runtime.precision == 'bf16'",message="TensorRT-LLM supports only bf16 precision in v1"
type InferenceDeploymentSpec struct {
	Model         ModelSpec         `json:"model"`
	Runtime       RuntimeSpec       `json:"runtime"`
	Accelerator   AcceleratorSpec   `json:"accelerator"`
	Scaling       ScalingSpec       `json:"scaling"`
	SLO           *SLOSpec          `json:"slo,omitempty"`
	Admission     AdmissionSpec     `json:"admission"`
	Routing       RoutingSpec       `json:"routing"`
	Rollout       RolloutSpec       `json:"rollout"`
	Observability ObservabilitySpec `json:"observability"`
}

type ModelSpec struct {
	// +kubebuilder:validation:Pattern=`^hf://[A-Za-z0-9][A-Za-z0-9._-]*/[A-Za-z0-9][A-Za-z0-9._-]*$`
	URI string `json:"uri"`
	// Lowercase is canonical so revision digests and node-cache keys cannot
	// diverge solely because clients used different hexadecimal casing.
	// +kubebuilder:validation:Pattern=`^[0-9a-f]{40}$`
	Revision string `json:"revision"`
}

type RuntimeSpec struct {
	// +kubebuilder:default=auto
	Backend RuntimeBackend `json:"backend"`
	// +kubebuilder:validation:Enum=bf16;fp8
	// +kubebuilder:default=bf16
	Precision string `json:"precision"`
	// +kubebuilder:validation:Enum=none;awq
	// +kubebuilder:default=none
	Quantization string `json:"quantization,omitempty"`
	// +kubebuilder:validation:Enum=1;2;4
	TensorParallelism int32 `json:"tensorParallelism"`
	// +kubebuilder:validation:Minimum=1
	MaxModelLen   int32             `json:"maxModelLen"`
	PrefixCaching PrefixCachingSpec `json:"prefixCaching"`
}

type PrefixCachingSpec struct {
	Enabled bool `json:"enabled"`
}

type AcceleratorSpec struct {
	// +kubebuilder:validation:Enum=nvidia
	Vendor string `json:"vendor"`
	// +kubebuilder:validation:MinLength=1
	Type string `json:"type"`
	// +kubebuilder:validation:Enum=1;2;4
	Count int32 `json:"count"`
}

type ScalingSpec struct {
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:default=1
	MinReplicas int32 `json:"minReplicas"`
	// +kubebuilder:validation:Minimum=1
	MaxReplicas int32 `json:"maxReplicas"`
	// +kubebuilder:validation:Enum=saturation
	// +kubebuilder:default=saturation
	Policy string `json:"policy"`
}

type SLOSpec struct {
	TTFT MetricSLO `json:"ttft"`
	TPOT MetricSLO `json:"tpot"`
}

type MetricSLO struct {
	// +kubebuilder:validation:Minimum=1
	// +kubebuilder:validation:Maximum=100
	Percentile int32 `json:"percentile"`
	// +kubebuilder:validation:Minimum=1
	TargetMS int64 `json:"targetMs"`
}

type AdmissionSpec struct {
	// +kubebuilder:validation:Minimum=1
	MaxConcurrentRequests int32 `json:"maxConcurrentRequests"`
	// +kubebuilder:validation:Minimum=0
	MaxQueuedRequests int32 `json:"maxQueuedRequests"`
	// +kubebuilder:validation:Enum=interactive;standard;batch
	// +kubebuilder:default=standard
	PriorityClass string `json:"priorityClass"`
}

type RoutingSpec struct {
	// +kubebuilder:default=load-aware
	Policy RoutingPolicy `json:"policy"`
}

type RolloutSpec struct {
	// +kubebuilder:validation:Enum=progressive
	// +kubebuilder:default=progressive
	Strategy string `json:"strategy"`
	// +kubebuilder:validation:Minimum=0
	// +kubebuilder:validation:Maximum=100
	// +kubebuilder:default=10
	ShadowPercent int32 `json:"shadowPercent"`
}

type ObservabilitySpec struct {
	// +kubebuilder:default=true
	Tracing bool `json:"tracing"`
}

type InferenceDeploymentStatus struct {
	Phase              DeploymentPhase    `json:"phase,omitempty"`
	ObservedGeneration int64              `json:"observedGeneration,omitempty"`
	Endpoint           EndpointStatus     `json:"endpoint,omitempty"`
	Runtime            RuntimeStatus      `json:"runtime,omitempty"`
	Replicas           ReplicaStatus      `json:"replicas,omitempty"`
	Revision           RevisionStatus     `json:"revision,omitempty"`
	Cache              CacheStatus        `json:"cache,omitempty"`
	Rollout            RolloutStatus      `json:"rollout,omitempty"`
	Conditions         []metav1.Condition `json:"conditions,omitempty"`
}

type EndpointStatus struct {
	URL string `json:"url,omitempty"`
}

type RuntimeStatus struct {
	Backend           RuntimeBackend `json:"backend,omitempty"`
	Version           string         `json:"version,omitempty"`
	ModelRevision     string         `json:"modelRevision,omitempty"`
	TensorParallelism int32          `json:"tensorParallelism,omitempty"`
}

type ReplicaStatus struct {
	Desired int32 `json:"desired,omitempty"`
	Ready   int32 `json:"ready,omitempty"`
}

type RevisionStatus struct {
	Stable      string `json:"stable,omitempty"`
	Candidate   string `json:"candidate,omitempty"`
	StableID    string `json:"stableId,omitempty"`
	CandidateID string `json:"candidateId,omitempty"`
	// LastFailed and LastFailedID retain the desired revision tombstone after
	// its 24-hour Kubernetes inspection window expires. They are historical
	// identity, not an active routing candidate.
	LastFailed   string `json:"lastFailed,omitempty"`
	LastFailedID string `json:"lastFailedId,omitempty"`
}

type CacheStatus struct {
	Weights     string `json:"weights,omitempty"`
	WorkersWarm int32  `json:"workersWarm,omitempty"`
}

type RolloutStatus struct {
	Stage                string                  `json:"stage,omitempty"`
	StageStartedAt       *metav1.Time            `json:"stageStartedAt,omitempty"`
	CandidateWeight      int32                   `json:"candidateWeight,omitempty"`
	RegressionWindows    int32                   `json:"regressionWindows,omitempty"`
	LastRegressionWindow *metav1.Time            `json:"lastRegressionWindow,omitempty"`
	LastRollback         *RollbackEvidenceStatus `json:"lastRollback,omitempty"`
}

// RollbackEvidenceStatus is a bounded, label-free snapshot of the signals
// that caused the most recent rollback. Raw PromQL, pod names, request IDs,
// and arbitrary metric labels are deliberately excluded from CR status.
type RollbackEvidenceStatus struct {
	// +kubebuilder:validation:Enum=endpoint-picker;shadow-runtime;unavailable
	Source string `json:"source"`
	// +kubebuilder:validation:MaxLength=32
	Stage                    string      `json:"stage"`
	ObservedAt               metav1.Time `json:"observedAt"`
	WindowStartedAt          metav1.Time `json:"windowStartedAt"`
	Available                bool        `json:"available"`
	CandidateRequests        int64       `json:"candidateRequests,omitempty"`
	StableErrorRatePPM       int64       `json:"stableErrorRatePpm,omitempty"`
	CandidateErrorRatePPM    int64       `json:"candidateErrorRatePpm,omitempty"`
	StableTTFTP95MS          int64       `json:"stableTtftP95Ms,omitempty"`
	CandidateTTFTP95MS       int64       `json:"candidateTtftP95Ms,omitempty"`
	StableTPOTP95MS          int64       `json:"stableTpotP95Ms,omitempty"`
	CandidateTPOTP95MS       int64       `json:"candidateTpotP95Ms,omitempty"`
	StableQueueP95MS         int64       `json:"stableQueueP95Ms,omitempty"`
	CandidateQueueP95MS      int64       `json:"candidateQueueP95Ms,omitempty"`
	CandidateOOMs            int64       `json:"candidateOoms,omitempty"`
	CandidateXIDErrors       int64       `json:"candidateXidErrors,omitempty"`
	CandidateRestarts        int64       `json:"candidateRestarts,omitempty"`
	CandidateSLOViolation    bool        `json:"candidateSloViolation,omitempty"`
	MaxCandidateErrorRatePPM int64       `json:"maxCandidateErrorRatePpm"`
	MaxErrorRateIncreasePPM  int64       `json:"maxErrorRateIncreasePpm"`
	MaxTTFTRatioPPM          int64       `json:"maxTtftRatioPpm"`
	MaxTPOTRatioPPM          int64       `json:"maxTpotRatioPpm"`
	MaxQueueRatioPPM         int64       `json:"maxQueueRatioPpm"`
	MaxRestarts              int64       `json:"maxRestarts"`
	// +kubebuilder:validation:MaxLength=256
	UnavailableReason string `json:"unavailableReason,omitempty"`
	// +kubebuilder:validation:MaxLength=256
	Reason string `json:"reason"`
}

// +kubebuilder:object:root=true
// +kubebuilder:subresource:status
// +kubebuilder:resource:shortName=infdeploy
// +kubebuilder:printcolumn:name="Phase",type=string,JSONPath=`.status.phase`
// +kubebuilder:printcolumn:name="Backend",type=string,JSONPath=`.status.runtime.backend`
// +kubebuilder:printcolumn:name="Ready",type=integer,JSONPath=`.status.replicas.ready`
// +kubebuilder:printcolumn:name="Age",type=date,JSONPath=`.metadata.creationTimestamp`
type InferenceDeployment struct {
	metav1.TypeMeta   `json:",inline"`
	metav1.ObjectMeta `json:"metadata,omitempty"`

	Spec   InferenceDeploymentSpec   `json:"spec"`
	Status InferenceDeploymentStatus `json:"status,omitempty"`
}

// +kubebuilder:object:root=true
type InferenceDeploymentList struct {
	metav1.TypeMeta `json:",inline"`
	metav1.ListMeta `json:"metadata,omitempty"`
	Items           []InferenceDeployment `json:"items"`
}
