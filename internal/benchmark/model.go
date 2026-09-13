// Package benchmark owns reproducible benchmark runs and the measured runtime
// profiles that are eligible for backend:auto selection.
package benchmark

import (
	"encoding/json"
	"time"
)

type Backend string

const (
	BackendVLLM        Backend = "vllm"
	BackendTensorRTLLM Backend = "tensorrt-llm"
)

type ProfileKey struct {
	ModelURI              string  `json:"model_uri"`
	ModelRevision         string  `json:"model_revision"`
	Backend               Backend `json:"backend"`
	BackendVersion        string  `json:"backend_version"`
	RuntimeImageDigest    string  `json:"runtime_image_digest"`
	GPUSKU                string  `json:"gpu_sku"`
	GPUCount              int32   `json:"gpu_count"`
	Precision             string  `json:"precision"`
	Quantization          string  `json:"quantization"`
	TensorParallelism     int32   `json:"tensor_parallelism"`
	MaxContextBucket      int32   `json:"max_context_bucket"`
	DriverCUDAFingerprint string  `json:"driver_cuda_fingerprint"`
	ScenarioDigest        string  `json:"scenario_digest"`
}

type Measurements struct {
	TTFTP50MS                float64 `json:"ttft_p50_ms"`
	TTFTP95MS                float64 `json:"ttft_p95_ms"`
	TTFTP99MS                float64 `json:"ttft_p99_ms"`
	TPOTP50MS                float64 `json:"tpot_p50_ms"`
	TPOTP95MS                float64 `json:"tpot_p95_ms"`
	TPOTP99MS                float64 `json:"tpot_p99_ms"`
	E2EP95MS                 float64 `json:"e2e_p95_ms"`
	QueueP95MS               float64 `json:"queue_p95_ms"`
	InputTokensPerSecond     float64 `json:"input_tokens_per_second"`
	OutputTokensPerSecond    float64 `json:"output_tokens_per_second"`
	GPUHours                 float64 `json:"gpu_hours"`
	CostPerMillionInput      float64 `json:"cost_per_million_input_tokens"`
	CostPerMillionOutput     float64 `json:"cost_per_million_output_tokens"`
	CostPerSuccessfulRequest float64 `json:"cost_per_successful_request"`
	SLOAttainmentPercent     float64 `json:"slo_attainment_percent"`
}

type RuntimeProfile struct {
	ID             string       `json:"id"`
	Key            ProfileKey   `json:"key"`
	Measurements   Measurements `json:"measurements"`
	BenchmarkRunID string       `json:"benchmark_run_id"`
	Eligible       bool         `json:"eligible"`
	ApprovedBy     string       `json:"approved_by,omitempty"`
	ApprovedAt     *time.Time   `json:"approved_at,omitempty"`
	MeasuredAt     time.Time    `json:"measured_at"`
}

type RunState string

const (
	// Run state values intentionally match deployment.BenchmarkState and the
	// persisted benchmark_runs values. The control API and the scheduler are
	// two views over one state machine, not independent benchmark stores.
	RunPending   RunState = "queued"
	RunRunning   RunState = "running"
	RunSucceeded RunState = "succeeded"
	RunFailed    RunState = "failed"
)

type Run struct {
	ID             string          `json:"id"`
	TenantID       string          `json:"tenant_id"`
	DeploymentID   string          `json:"deployment_id"`
	RevisionID     string          `json:"revision_id"`
	ScenarioName   string          `json:"scenario_name"`
	ScenarioDigest string          `json:"scenario_digest"`
	Configuration  json.RawMessage `json:"configuration,omitempty"`
	State          RunState        `json:"state"`
	Provenance     map[string]any  `json:"provenance,omitempty"`
	Result         *Measurements   `json:"result,omitempty"`
	ArtifactURI    string          `json:"artifact_uri,omitempty"`
	Error          string          `json:"error,omitempty"`
	RentalPriceUSD float64         `json:"rental_price_usd_per_gpu_hour"`
	Namespace      string          `json:"-"`
	DeploymentName string          `json:"-"`
	// RuntimePodPrefix is resolved from the controller-projected stable or
	// candidate revision name before the execution snapshot is frozen.
	RuntimePodPrefix string          `json:"-"`
	EPPService       string          `json:"-"`
	Serving          ServingContract `json:"-"`
	// Execution is an immutable, server-authored snapshot. It is persisted
	// before the Kubernetes Job is created, so callback validation never trusts
	// deployment/runtime/provenance fields supplied by the benchmark caller.
	Execution              ExecutionContract `json:"-"`
	ExecutionConfiguration json.RawMessage   `json:"-"`
	ExecutionDigest        string            `json:"-"`
	IdempotencyKey         string            `json:"-"`
	CreatedAt              time.Time         `json:"created_at"`
	StartedAt              *time.Time        `json:"started_at,omitempty"`
	CompletedAt            *time.Time        `json:"completed_at,omitempty"`
}

// ResultReport is the authenticated callback contract used by one benchmark
// Job. Successful reports always carry the exact profile compatibility key;
// profiles are persisted as ineligible until an operator approves them.
type ResultReport struct {
	State                       RunState       `json:"state"`
	Measurements                *Measurements  `json:"measurements,omitempty"`
	ProfileKey                  *ProfileKey    `json:"profile_key,omitempty"`
	Provenance                  map[string]any `json:"provenance,omitempty"`
	ScenarioConfigurationDigest string         `json:"scenario_configuration_digest,omitempty"`
	ArtifactURI                 string         `json:"artifact_uri,omitempty"`
	Error                       string         `json:"error,omitempty"`
}

// ServingContract is the immutable deployment revision selected for a run.
// It is loaded by the repository from deployment_revisions rather than from
// the caller-controlled benchmark scenario document.
type ServingContract struct {
	ModelURI           string
	ModelRevision      string
	Backend            Backend
	RuntimeImageDigest string
	GPUSKU             string
	GPUCount           int32
	Precision          string
	Quantization       string
	TensorParallelism  int32
	MaxContextBucket   int32
	PrefixCaching      bool
	// RoutingPolicy is the current deployment policy observed when the run is
	// prepared. It is not part of the immutable runtime profile key, but it is
	// part of the standardized scenario digest and therefore must never come
	// from caller-authored benchmark JSON.
	RoutingPolicy string
}

// ExecutionContract captures the operator-controlled environment in which a
// scheduled benchmark is allowed to run. Runner and runtime image identities
// are separated deliberately: backend selection is keyed by the runtime image,
// while the runner digest proves which measurement implementation executed.
type ExecutionContract struct {
	Provider                string `json:"provider"`
	BenchmarkTool           string `json:"benchmark_tool"`
	RoutingPolicy           string `json:"routing_policy"`
	RuntimePodPrefix        string `json:"runtime_pod_prefix"`
	EPPService              string `json:"epp_service"`
	InferenceBaseURL        string `json:"inference_base_url"`
	PrometheusURL           string `json:"prometheus_url"`
	RuntimeVersion          string `json:"runtime_version"`
	RuntimeImageDigest      string `json:"runtime_image_digest"`
	RunnerImageDigest       string `json:"runner_image_digest"`
	DriverVersion           string `json:"driver_version"`
	CUDAVersion             string `json:"cuda_version"`
	DatasetDigest           string `json:"dataset_digest"`
	SelectionScenarioDigest string `json:"selection_scenario_digest"`
}

type SLO struct {
	TTFTP95MS float64
	TPOTP95MS float64
}

type SelectionRequest struct {
	KeyWithoutBackend ProfileKey
	// ExpectedRuntimes contains the exact pinned runtime identity for each
	// backend being compared. Runtime versions and image digests are backend
	// specific, so a single value in KeyWithoutBackend cannot safely compare
	// vLLM with TensorRT-LLM.
	ExpectedRuntimes map[Backend]RuntimeIdentity
	SLO              *SLO
}

type RuntimeIdentity struct {
	BackendVersion     string
	RuntimeImageDigest string
}

type Selection struct {
	Backend      Backend
	ProfileID    string
	Status       string
	Measurements *Measurements
}
