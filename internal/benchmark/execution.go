package benchmark

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"net/url"
	"regexp"
	"strings"
)

var (
	immutableDigestPattern = regexp.MustCompile(`^sha256:[0-9a-f]{64}$`)
	driverVersionPattern   = regexp.MustCompile(`^[0-9]{3,4}\.[0-9]{1,3}(?:\.[0-9]{1,3})?$`)
	cudaVersionPattern     = regexp.MustCompile(`^[0-9]{1,2}\.[0-9]{1,2}(?:\.[0-9]+)?$`)
	scenarioNamePattern    = regexp.MustCompile(`^[a-z0-9][a-z0-9._-]{0,127}$`)
)

var pinnedDatasetDigests = map[string]string{
	"prefix-reuse.jsonl": "cf4d63dcba48edd38e06a8cd5394e11c5d7950a02a48cd1180906ecfec1e9e4e",
	"smoke.jsonl":        "85165e6dd57f1153a8dc5ce2209aeeebe6da0e6762a8987fcbddfd6176e18cc1",
}

// ScheduledBenchmarkTool is part of the immutable benchmark compatibility
// contract. Native HTTP runs remain useful for local correctness and bespoke
// routing tests, but cannot produce an approvable backend-selection profile.
const ScheduledBenchmarkTool = "guidellm-0.7.0"

const syntheticDatasetIdentity = "guidellm:synthetic_text:exact"

// ExecutionAuthority is API-process configuration used exactly once to
// prepare a run. The resulting ExecutionContract is persisted before the Job
// is submitted and remains stable across scheduler/process restarts.
type ExecutionAuthority struct {
	Provider         string
	InferenceBaseURL string
	PrometheusURL    string
	DriverVersion    string
	CUDAVersion      string
	RunnerImage      string
	RuntimeVersions  map[Backend]string
}

func (a ExecutionAuthority) Prepare(run Run) (Run, error) {
	if len(run.ExecutionConfiguration) > 0 || run.ExecutionDigest != "" {
		if len(run.ExecutionConfiguration) == 0 || !json.Valid(run.ExecutionConfiguration) ||
			!isLowerHex(run.ExecutionDigest, 64) {
			return Run{}, fmt.Errorf("%w: persisted benchmark execution snapshot is incomplete", ErrInvalidReport)
		}
		if ConfigurationDigest(run.ExecutionConfiguration) != run.ExecutionDigest {
			return Run{}, fmt.Errorf("%w: persisted benchmark execution digest does not match its configuration", ErrInvalidReport)
		}
		if err := run.Execution.Validate(run.Serving); err != nil {
			return Run{}, err
		}
		datasetDigest, selectionDigest, err := selectionScenarioDigest(run.ExecutionConfiguration)
		if err != nil || datasetDigest != run.Execution.DatasetDigest || selectionDigest != run.Execution.SelectionScenarioDigest {
			return Run{}, fmt.Errorf("%w: persisted benchmark selection contract is corrupt", ErrInvalidReport)
		}
		return run, nil
	}

	runtimeVersion := strings.TrimSpace(a.RuntimeVersions[run.Serving.Backend])
	contract := ExecutionContract{
		Provider:           strings.TrimSpace(a.Provider),
		BenchmarkTool:      ScheduledBenchmarkTool,
		RoutingPolicy:      run.Serving.RoutingPolicy,
		RuntimePodPrefix:   strings.TrimSpace(run.RuntimePodPrefix),
		EPPService:         strings.TrimSpace(run.EPPService),
		InferenceBaseURL:   strings.TrimRight(strings.TrimSpace(a.InferenceBaseURL), "/"),
		PrometheusURL:      strings.TrimRight(strings.TrimSpace(a.PrometheusURL), "/"),
		RuntimeVersion:     runtimeVersion,
		RuntimeImageDigest: strings.TrimSpace(run.Serving.RuntimeImageDigest),
		RunnerImageDigest:  imageDigest(a.RunnerImage),
		DriverVersion:      strings.TrimSpace(a.DriverVersion),
		CUDAVersion:        strings.TrimSpace(a.CUDAVersion),
	}
	configuration, err := authoritativeScenario(run, contract)
	if err != nil {
		return Run{}, err
	}
	datasetDigest, selectionDigest, err := selectionScenarioDigest(configuration)
	if err != nil {
		return Run{}, err
	}
	contract.DatasetDigest = datasetDigest
	contract.SelectionScenarioDigest = selectionDigest
	if err := contract.Validate(run.Serving); err != nil {
		return Run{}, err
	}
	run.Execution = contract
	run.ExecutionConfiguration = configuration
	run.ExecutionDigest = ConfigurationDigest(configuration)
	return run, nil
}

func (c ExecutionContract) Validate(serving ServingContract) error {
	if strings.TrimSpace(c.Provider) == "" || c.Provider == "set-at-run-time" {
		return fmt.Errorf("%w: benchmark provider must be configured by the operator", ErrInvalidReport)
	}
	if c.BenchmarkTool != ScheduledBenchmarkTool {
		return fmt.Errorf("%w: scheduled benchmark tool must be %q", ErrInvalidReport, ScheduledBenchmarkTool)
	}
	if !supportedRoutingPolicy(c.RoutingPolicy) || c.RoutingPolicy != serving.RoutingPolicy {
		return fmt.Errorf("%w: frozen routing policy must match the scheduled deployment", ErrInvalidReport)
	}
	if c.RuntimePodPrefix == "" || kubeNameInvalid(c.RuntimePodPrefix) {
		return fmt.Errorf("%w: controller-observed runtime Pod prefix is required", ErrInvalidReport)
	}
	if c.EPPService == "" || kubeNameInvalid(c.EPPService) {
		return fmt.Errorf("%w: controller-observed endpoint-picker Service is required", ErrInvalidReport)
	}
	for name, value := range map[string]string{
		"runtime version": c.RuntimeVersion,
		"driver version":  c.DriverVersion,
		"CUDA version":    c.CUDAVersion,
	} {
		if strings.TrimSpace(value) == "" || value == "set-at-run-time" {
			return fmt.Errorf("%w: authoritative %s is required", ErrInvalidReport, name)
		}
	}
	if !driverVersionPattern.MatchString(c.DriverVersion) {
		return fmt.Errorf("%w: authoritative NVIDIA driver version is invalid", ErrInvalidReport)
	}
	if !cudaVersionPattern.MatchString(c.CUDAVersion) {
		return fmt.Errorf("%w: authoritative CUDA version is invalid", ErrInvalidReport)
	}
	if !immutableDigestPattern.MatchString(c.RuntimeImageDigest) || c.RuntimeImageDigest != serving.RuntimeImageDigest {
		return fmt.Errorf("%w: frozen runtime image must be a matching sha256 digest", ErrInvalidReport)
	}
	if !immutableDigestPattern.MatchString(c.RunnerImageDigest) {
		return fmt.Errorf("%w: benchmark runner image must be digest-pinned", ErrInvalidReport)
	}
	if !isLowerHex(c.DatasetDigest, 64) || !isLowerHex(c.SelectionScenarioDigest, 64) {
		return fmt.Errorf("%w: pinned dataset and selection-scenario digests are required", ErrInvalidReport)
	}
	for name, raw := range map[string]string{"inference base URL": c.InferenceBaseURL, "Prometheus URL": c.PrometheusURL} {
		parsed, err := url.Parse(raw)
		if err != nil || (parsed.Scheme != "http" && parsed.Scheme != "https") || parsed.Host == "" || parsed.User != nil {
			return fmt.Errorf("%w: authoritative %s must be an absolute HTTP(S) URL", ErrInvalidReport, name)
		}
	}
	if serving.ModelURI == "" || serving.ModelRevision == "" || serving.Backend == "" ||
		serving.GPUSKU == "" || serving.GPUCount <= 0 || serving.Precision == "" ||
		serving.TensorParallelism <= 0 || serving.MaxContextBucket <= 0 ||
		!supportedRoutingPolicy(serving.RoutingPolicy) {
		return fmt.Errorf("%w: immutable serving contract is incomplete", ErrInvalidReport)
	}
	return nil
}

func authoritativeScenario(run Run, contract ExecutionContract) (json.RawMessage, error) {
	if !scenarioNamePattern.MatchString(run.ScenarioName) {
		return nil, fmt.Errorf("%w: benchmark scenario name must be a lowercase filesystem-safe identifier", ErrInvalidReport)
	}
	if strings.TrimSpace(run.DeploymentID) == "" || strings.TrimSpace(run.DeploymentName) == "" {
		return nil, fmt.Errorf("%w: scheduled deployment identity is incomplete", ErrInvalidReport)
	}
	if !json.Valid(run.Configuration) {
		return nil, fmt.Errorf("%w: benchmark scenario configuration must be valid JSON", ErrInvalidReport)
	}
	var document map[string]any
	if err := json.Unmarshal(run.Configuration, &document); err != nil {
		return nil, fmt.Errorf("%w: decode benchmark scenario: %v", ErrInvalidReport, err)
	}
	if document["schema_version"] != float64(1) {
		return nil, fmt.Errorf("%w: benchmark scenario schema_version must be 1", ErrInvalidReport)
	}
	workload, ok := document["workload"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: benchmark scenario workload is required", ErrInvalidReport)
	}
	// Publishable selection runs use GuideLLM's exact synthetic generator.
	// Caller-selected prompt files cannot silently change tokenization or make
	// otherwise identical backend profiles incomparable.
	workload["dataset"] = "synthetic"
	routing, ok := document["routing"].(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%w: benchmark scenario routing is required", ErrInvalidReport)
	}
	if policy, _ := routing["policy"].(string); policy != run.Serving.RoutingPolicy {
		return nil, fmt.Errorf("%w: benchmark routing policy does not match the active deployment", ErrInvalidReport)
	}
	if cacheState, _ := document["cache_state"].(string); cacheState != "process-warm" {
		return nil, fmt.Errorf("%w: scheduled selection benchmarks require process-warm workers", ErrInvalidReport)
	}
	modelName := strings.TrimPrefix(run.Serving.ModelURI, "hf://")
	document["name"] = run.ScenarioName
	document["measurement_tool"] = ScheduledBenchmarkTool
	document["publishable"] = true
	document["target"] = map[string]any{
		"base_url":          contract.InferenceBaseURL,
		"deployment":        run.DeploymentName,
		"deployment_id":     run.DeploymentID,
		"api_mode":          "inferscale",
		"api_key_env":       "INFERSCALE_API_KEY",
		"verify_tls":        strings.HasPrefix(contract.InferenceBaseURL, "https://"),
		"request_timeout_s": 600,
	}
	document["deployment"] = map[string]any{
		"model":              modelName,
		"model_revision":     run.Serving.ModelRevision,
		"backend":            string(run.Serving.Backend),
		"backend_version":    contract.RuntimeVersion,
		"precision":          run.Serving.Precision,
		"quantization":       normalizedQuantization(run.Serving.Quantization),
		"gpu_type":           run.Serving.GPUSKU,
		"gpu_count":          run.Serving.GPUCount,
		"tensor_parallelism": run.Serving.TensorParallelism,
		"prefix_cache":       run.Serving.PrefixCaching,
		"max_model_len":      run.Serving.MaxContextBucket,
	}
	document["metadata"] = map[string]any{
		"provider":               contract.Provider,
		"runtime_version":        contract.RuntimeVersion,
		"container_image_digest": contract.RuntimeImageDigest,
		"runtime_image_digest":   contract.RuntimeImageDigest,
		"runner_image_digest":    contract.RunnerImageDigest,
		"driver_version":         contract.DriverVersion,
		"cuda_version":           contract.CUDAVersion,
		"gpu_hourly_price":       run.RentalPriceUSD,
		"benchmark_tool":         contract.BenchmarkTool,
	}
	encoded, err := json.Marshal(document)
	if err != nil {
		return nil, fmt.Errorf("encode authoritative benchmark scenario: %w", err)
	}
	return encoded, nil
}

func selectionScenarioDigest(configuration []byte) (string, string, error) {
	var scenario struct {
		SchemaVersion   int    `json:"schema_version"`
		MeasurementTool string `json:"measurement_tool"`
		Publishable     bool   `json:"publishable"`
		Workload        struct {
			InputTokens  int    `json:"input_tokens"`
			OutputTokens int    `json:"output_tokens"`
			Concurrency  int    `json:"concurrency"`
			Requests     int    `json:"requests"`
			Dataset      string `json:"dataset"`
			Seed         *int   `json:"seed"`
		} `json:"workload"`
		CacheState string `json:"cache_state"`
		Routing    struct {
			Policy string `json:"policy"`
		} `json:"routing"`
		Experiment map[string]any `json:"experiment"`
	}
	if err := json.Unmarshal(configuration, &scenario); err != nil {
		return "", "", fmt.Errorf("%w: decode prepared selection scenario: %v", ErrInvalidReport, err)
	}
	if scenario.MeasurementTool != ScheduledBenchmarkTool || !scenario.Publishable {
		return "", "", fmt.Errorf("%w: scheduled selection scenarios require publishable %s measurements", ErrInvalidReport, ScheduledBenchmarkTool)
	}
	if scenario.SchemaVersion != 1 || scenario.Workload.InputTokens <= 0 || scenario.Workload.OutputTokens <= 0 ||
		scenario.Workload.Concurrency <= 0 || scenario.Workload.Requests <= 0 ||
		scenario.Workload.Concurrency > scenario.Workload.Requests {
		return "", "", fmt.Errorf("%w: scheduled workload token, concurrency, and request counts are invalid", ErrInvalidReport)
	}
	if scenario.CacheState != "remote-cold" && scenario.CacheState != "cache-warm" && scenario.CacheState != "process-warm" {
		return "", "", fmt.Errorf("%w: scheduled cache_state is invalid", ErrInvalidReport)
	}
	if scenario.Routing.Policy != "round-robin" && scenario.Routing.Policy != "load-aware" && scenario.Routing.Policy != "prefix-aware" {
		return "", "", fmt.Errorf("%w: scheduled routing policy is invalid", ErrInvalidReport)
	}
	if scenario.Workload.Dataset != "synthetic" {
		return "", "", fmt.Errorf("%w: scheduled GuideLLM workload must use exact synthetic data", ErrInvalidReport)
	}
	datasetSum := sha256.Sum256([]byte(syntheticDatasetIdentity))
	datasetDigest := hex.EncodeToString(datasetSum[:])
	seed := 1
	if scenario.Workload.Seed != nil {
		seed = *scenario.Workload.Seed
	}
	contract := map[string]any{
		"schema_version": scenario.SchemaVersion, "measurement_tool": scenario.MeasurementTool, "publishable": scenario.Publishable,
		"workload": map[string]any{
			"input_tokens": scenario.Workload.InputTokens, "output_tokens": scenario.Workload.OutputTokens,
			"concurrency": scenario.Workload.Concurrency, "requests": scenario.Workload.Requests,
			"seed": seed, "dataset_identity": syntheticDatasetIdentity,
		},
		"cache_state": scenario.CacheState, "routing_policy": scenario.Routing.Policy,
		"experiment": scenario.Experiment,
	}
	if contract["experiment"] == nil {
		contract["experiment"] = map[string]any{}
	}
	var encoded bytes.Buffer
	encoder := json.NewEncoder(&encoded)
	encoder.SetEscapeHTML(false)
	if err := encoder.Encode(contract); err != nil {
		return "", "", err
	}
	selection := sha256.Sum256(bytes.TrimSuffix(encoded.Bytes(), []byte("\n")))
	return datasetDigest, hex.EncodeToString(selection[:]), nil
}

func ConfigurationDigest(configuration []byte) string {
	sum := sha256.Sum256(configuration)
	return hex.EncodeToString(sum[:])
}

func kubeNameInvalid(value string) bool {
	if len(value) == 0 || len(value) > 63 || !lowerAlphaNumeric(value[0]) {
		return true
	}
	for index := 1; index < len(value); index++ {
		character := value[index]
		if lowerAlphaNumeric(character) || (character == '-' && index < len(value)-1) {
			continue
		}
		return true
	}
	return false
}

func lowerAlphaNumeric(character byte) bool {
	return (character >= 'a' && character <= 'z') || (character >= '0' && character <= '9')
}

func DriverCUDAFingerprint(driverVersion, cudaVersion string) string {
	sum := sha256.Sum256([]byte("driver=" + driverVersion + "|cuda=" + cudaVersion))
	return hex.EncodeToString(sum[:])
}

func imageDigest(reference string) string {
	index := strings.LastIndex(reference, "@sha256:")
	if index < 0 {
		return ""
	}
	return reference[index+1:]
}

func normalizedQuantization(value string) string {
	if strings.TrimSpace(value) == "" {
		return "none"
	}
	return value
}

func supportedRoutingPolicy(value string) bool {
	switch value {
	case "round-robin", "load-aware", "prefix-aware":
		return true
	default:
		return false
	}
}
