package benchmark

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/google/uuid"
)

var (
	ErrInvalidTransition = errors.New("invalid benchmark run transition")
	ErrInvalidReport     = errors.New("invalid benchmark result report")
	ErrRunNotFound       = errors.New("benchmark run not found")
)

// Repository is the single persistence contract shared by the public
// benchmark API, scheduler, callback ingester, and operator profile approval.
// CompleteRun must update the run, append benchmark_results, and (when non-nil)
// append the measured profile in one transaction.
type Repository interface {
	CreateRun(ctx context.Context, run Run) error
	GetRun(ctx context.Context, id string) (Run, error)
	ListRuns(ctx context.Context, tenantID, deploymentID, cursor string, limit int) ([]Run, string, error)
	CompleteRun(ctx context.Context, runID string, result Measurements, provenance map[string]any, artifactURI string, profile *RuntimeProfile, at time.Time) (Run, error)
	MarkRunFailed(ctx context.Context, runID, reason string) error
	ApproveProfile(ctx context.Context, profileID, operator string, at time.Time) error
}

type IDGenerator func() string

type Service struct {
	repository Repository
	newID      IDGenerator
	now        func() time.Time
}

func NewService(repository Repository, newID IDGenerator) *Service {
	if newID == nil {
		newID = NewID
	}
	return &Service{repository: repository, newID: newID, now: time.Now}
}

func NewID() string {
	id, err := uuid.NewV7()
	if err != nil {
		return uuid.NewString()
	}
	return id.String()
}

func (s *Service) StartRun(ctx context.Context, run Run) (Run, error) {
	if run.TenantID == "" || run.DeploymentID == "" || run.RevisionID == "" || run.ScenarioName == "" || run.ScenarioDigest == "" {
		return Run{}, fmt.Errorf("%w: tenant, deployment, revision, scenario name, and scenario digest are required", ErrInvalidReport)
	}
	run.ID = s.newID()
	run.State = RunPending
	run.CreatedAt = s.now().UTC()
	if err := s.repository.CreateRun(ctx, run); err != nil {
		return Run{}, fmt.Errorf("create benchmark run: %w", err)
	}
	return run, nil
}

// CompleteRun persists a result without producing a backend-selection profile.
// The authenticated Job callback uses IngestResult, which requires a key and
// appends a profile atomically with the result.
func (s *Service) CompleteRun(ctx context.Context, id string, result Measurements, provenance map[string]any, artifactURI string) (Run, error) {
	if err := validateMeasurements(result); err != nil {
		return Run{}, err
	}
	return s.repository.CompleteRun(ctx, id, result, provenance, artifactURI, nil, s.now().UTC())
}

func (s *Service) IngestResult(ctx context.Context, id string, report ResultReport) (Run, error) {
	if strings.TrimSpace(id) == "" {
		return Run{}, fmt.Errorf("%w: run ID is required", ErrInvalidReport)
	}
	switch report.State {
	case RunFailed:
		reason := strings.TrimSpace(report.Error)
		if reason == "" {
			return Run{}, fmt.Errorf("%w: failed reports require an error", ErrInvalidReport)
		}
		if err := s.repository.MarkRunFailed(ctx, id, reason); err != nil {
			return Run{}, err
		}
		return s.repository.GetRun(ctx, id)
	case RunSucceeded:
		if report.Measurements == nil || report.ProfileKey == nil {
			return Run{}, fmt.Errorf("%w: successful reports require measurements and profile_key", ErrInvalidReport)
		}
		if err := validateMeasurements(*report.Measurements); err != nil {
			return Run{}, err
		}
		if err := validateProfileKey(*report.ProfileKey); err != nil {
			return Run{}, err
		}
		run, err := s.repository.GetRun(ctx, id)
		if err != nil {
			return Run{}, err
		}
		if err := validateAuthoritativeReport(run, report); err != nil {
			return Run{}, err
		}
		measuredAt := s.now().UTC()
		profile := &RuntimeProfile{
			ID:             s.newID(),
			Key:            *report.ProfileKey,
			Measurements:   *report.Measurements,
			BenchmarkRunID: id,
			Eligible:       false,
			MeasuredAt:     measuredAt,
		}
		return s.repository.CompleteRun(ctx, id, *report.Measurements, report.Provenance, strings.TrimSpace(report.ArtifactURI), profile, measuredAt)
	default:
		return Run{}, fmt.Errorf("%w: state must be %q or %q", ErrInvalidReport, RunSucceeded, RunFailed)
	}
}

func validateAuthoritativeReport(run Run, report ResultReport) error {
	if run.State != RunRunning {
		return ErrInvalidTransition
	}
	if len(run.ExecutionConfiguration) == 0 || !isLowerHex(run.ExecutionDigest, 64) ||
		ConfigurationDigest(run.ExecutionConfiguration) != run.ExecutionDigest {
		return fmt.Errorf("%w: run has no valid authoritative execution snapshot", ErrInvalidReport)
	}
	if err := run.Execution.Validate(run.Serving); err != nil {
		return err
	}
	datasetDigest, selectionDigest, err := selectionScenarioDigest(run.ExecutionConfiguration)
	if err != nil || datasetDigest != run.Execution.DatasetDigest || selectionDigest != run.Execution.SelectionScenarioDigest {
		return fmt.Errorf("%w: authoritative scenario selection evidence is corrupt", ErrInvalidReport)
	}
	if report.ScenarioConfigurationDigest != run.ExecutionDigest {
		return fmt.Errorf("%w: scenario configuration does not match the scheduled run", ErrInvalidReport)
	}
	key := *report.ProfileKey
	if key.ModelURI != run.Serving.ModelURI || key.ModelRevision != run.Serving.ModelRevision ||
		key.Backend != run.Serving.Backend || key.BackendVersion != run.Execution.RuntimeVersion ||
		key.RuntimeImageDigest != run.Execution.RuntimeImageDigest || key.GPUSKU != run.Serving.GPUSKU ||
		key.GPUCount != run.Serving.GPUCount || key.Precision != run.Serving.Precision ||
		key.Quantization != normalizedQuantization(run.Serving.Quantization) ||
		key.TensorParallelism != run.Serving.TensorParallelism || key.MaxContextBucket != run.Serving.MaxContextBucket ||
		key.DriverCUDAFingerprint != DriverCUDAFingerprint(run.Execution.DriverVersion, run.Execution.CUDAVersion) ||
		key.ScenarioDigest != run.Execution.SelectionScenarioDigest {
		return fmt.Errorf("%w: measured profile does not match the scheduled serving/environment contract", ErrInvalidReport)
	}
	expectedProvenance := map[string]string{
		"provider":                run.Execution.Provider,
		"runtime_version":         run.Execution.RuntimeVersion,
		"container_image_digest":  run.Execution.RuntimeImageDigest,
		"runtime_image_digest":    run.Execution.RuntimeImageDigest,
		"runner_image_digest":     run.Execution.RunnerImageDigest,
		"nvidia_driver_version":   run.Execution.DriverVersion,
		"cuda_version":            run.Execution.CUDAVersion,
		"driver_cuda_fingerprint": DriverCUDAFingerprint(run.Execution.DriverVersion, run.Execution.CUDAVersion),
		"benchmark_tool":          run.Execution.BenchmarkTool,
	}
	for field, expected := range expectedProvenance {
		actual, ok := report.Provenance[field].(string)
		if !ok || actual != expected {
			return fmt.Errorf("%w: provenance field %s does not match the scheduled run", ErrInvalidReport, field)
		}
	}
	if err := validateProvenanceShape(report.Provenance, run); err != nil {
		return err
	}
	return nil
}

func validateProvenanceShape(provenance map[string]any, run Run) error {
	allowed := map[string]struct{}{
		"timestamp_utc": {}, "git_commit": {}, "git_dirty": {}, "hostname": {}, "kernel": {},
		"platform": {}, "python": {}, "nvidia_gpus": {}, "nvidia_driver_version": {},
		"cuda_version": {}, "driver_cuda_fingerprint": {}, "container_image_digest": {},
		"runtime_image_digest": {}, "runtime_version": {}, "runner_image_digest": {},
		"benchmark_tool": {}, "gpu_hourly_price": {}, "provider": {}, "telemetry_evidence": {},
	}
	for field := range provenance {
		if _, ok := allowed[field]; !ok {
			return fmt.Errorf("%w: unsupported provenance field %s", ErrInvalidReport, field)
		}
	}
	for _, field := range []string{"timestamp_utc", "hostname", "kernel", "platform", "python"} {
		value, ok := provenance[field].(string)
		if !ok || strings.TrimSpace(value) == "" || len(value) > 512 {
			return fmt.Errorf("%w: provenance field %s is missing or invalid", ErrInvalidReport, field)
		}
	}
	if _, err := time.Parse(time.RFC3339Nano, provenance["timestamp_utc"].(string)); err != nil {
		return fmt.Errorf("%w: provenance timestamp is invalid", ErrInvalidReport)
	}
	if value := provenance["git_commit"]; value != nil {
		commit, ok := value.(string)
		if !ok || !isLowerHex(commit, 40) {
			return fmt.Errorf("%w: provenance git commit is invalid", ErrInvalidReport)
		}
	}
	if value := provenance["git_dirty"]; value != nil {
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("%w: provenance git_dirty must be boolean or null", ErrInvalidReport)
		}
	}
	gpus, ok := provenance["nvidia_gpus"].([]any)
	if !ok || len(gpus) != int(run.Serving.GPUCount) || len(gpus) > 8 {
		return fmt.Errorf("%w: provenance GPU inventory is invalid", ErrInvalidReport)
	}
	for _, value := range gpus {
		entry, ok := value.(string)
		if !ok || strings.TrimSpace(entry) == "" || len(entry) > 256 || !gpuModelMatches(entry, run.Serving.GPUSKU) {
			return fmt.Errorf("%w: provenance GPU inventory entry is invalid", ErrInvalidReport)
		}
	}
	price, ok := provenance["gpu_hourly_price"].(float64)
	if !ok || math.IsNaN(price) || math.IsInf(price, 0) || math.Abs(price-run.RentalPriceUSD) > 1e-9 {
		return fmt.Errorf("%w: provenance GPU hourly price does not match the scheduled run", ErrInvalidReport)
	}
	if err := validateTelemetryEvidence(provenance["telemetry_evidence"], run); err != nil {
		return err
	}
	return nil
}

var requiredBenchmarkTelemetry = map[string]struct{}{
	"gpu_memory_used_bytes": {}, "gpu_power_watts": {}, "gpu_utilization": {},
	"kv_cache_utilization": {}, "queue_p95_seconds": {}, "running_requests": {}, "waiting_requests": {},
}

func validateTelemetryEvidence(raw any, run Run) error {
	evidence, ok := raw.(map[string]any)
	if !ok || len(evidence) != 5 {
		return fmt.Errorf("%w: complete telemetry_evidence is required", ErrInvalidReport)
	}
	for field := range evidence {
		switch field {
		case "valid", "required_queries", "expected_gpu_count", "expected_gpu_sku", "observed_gpus":
		default:
			return fmt.Errorf("%w: unsupported telemetry_evidence field %s", ErrInvalidReport, field)
		}
	}
	valid, ok := evidence["valid"].(bool)
	if !ok || !valid {
		return fmt.Errorf("%w: telemetry evidence is not valid", ErrInvalidReport)
	}
	count, ok := evidence["expected_gpu_count"].(float64)
	if !ok || count != float64(run.Serving.GPUCount) {
		return fmt.Errorf("%w: telemetry GPU count does not match the serving contract", ErrInvalidReport)
	}
	sku, ok := evidence["expected_gpu_sku"].(string)
	if !ok || sku != run.Serving.GPUSKU {
		return fmt.Errorf("%w: telemetry GPU SKU does not match the serving contract", ErrInvalidReport)
	}
	queries, ok := evidence["required_queries"].([]any)
	if !ok || len(queries) != len(requiredBenchmarkTelemetry) {
		return fmt.Errorf("%w: telemetry required-query evidence is incomplete", ErrInvalidReport)
	}
	seenQueries := make(map[string]struct{}, len(queries))
	for _, rawQuery := range queries {
		query, ok := rawQuery.(string)
		if !ok {
			return fmt.Errorf("%w: telemetry query identity is invalid", ErrInvalidReport)
		}
		if _, required := requiredBenchmarkTelemetry[query]; !required {
			return fmt.Errorf("%w: unsupported telemetry query %s", ErrInvalidReport, query)
		}
		if _, duplicate := seenQueries[query]; duplicate {
			return fmt.Errorf("%w: duplicate telemetry query %s", ErrInvalidReport, query)
		}
		seenQueries[query] = struct{}{}
	}
	observed, ok := evidence["observed_gpus"].([]any)
	if !ok || len(observed) != int(run.Serving.GPUCount) {
		return fmt.Errorf("%w: observed GPU evidence count does not match the serving contract", ErrInvalidReport)
	}
	seenUUIDs := make(map[string]struct{}, len(observed))
	for _, rawGPU := range observed {
		gpu, ok := rawGPU.(map[string]any)
		if !ok || len(gpu) != 4 {
			return fmt.Errorf("%w: observed GPU evidence entry is invalid", ErrInvalidReport)
		}
		for field := range gpu {
			switch field {
			case "uuid", "model", "pod", "namespace":
			default:
				return fmt.Errorf("%w: unsupported observed GPU field %s", ErrInvalidReport, field)
			}
		}
		uuid, uuidOK := gpu["uuid"].(string)
		model, modelOK := gpu["model"].(string)
		pod, podOK := gpu["pod"].(string)
		namespace, namespaceOK := gpu["namespace"].(string)
		if !uuidOK || !modelOK || !podOK || !namespaceOK || uuid == "" || len(uuid) > 128 ||
			len(model) > 256 || !gpuModelMatches(model, run.Serving.GPUSKU) ||
			namespace != run.Namespace || !strings.HasPrefix(pod, run.Execution.RuntimePodPrefix+"-") || len(pod) > 253 {
			return fmt.Errorf("%w: observed GPU identity does not match the scheduled runtime", ErrInvalidReport)
		}
		if _, duplicate := seenUUIDs[uuid]; duplicate {
			return fmt.Errorf("%w: observed GPU UUIDs must be unique", ErrInvalidReport)
		}
		seenUUIDs[uuid] = struct{}{}
	}
	return nil
}

func gpuModelMatches(observed, expected string) bool {
	normalize := func(value string) string {
		value = strings.ToLower(value)
		for _, prefix := range []string{"nvidia", "geforce"} {
			value = strings.ReplaceAll(value, prefix, "")
		}
		return strings.NewReplacer("_", "", "-", "", " ", "").Replace(value)
	}
	want := normalize(expected)
	return want != "" && strings.Contains(normalize(observed), want)
}

func (s *Service) ApproveProfile(ctx context.Context, profileID, operator string) error {
	if strings.TrimSpace(profileID) == "" || strings.TrimSpace(operator) == "" {
		return fmt.Errorf("%w: profile and operator are required", ErrInvalidReport)
	}
	return s.repository.ApproveProfile(ctx, profileID, operator, s.now().UTC())
}

func validateProfileKey(key ProfileKey) error {
	if key.ModelURI == "" || key.ModelRevision == "" || key.BackendVersion == "" || key.RuntimeImageDigest == "" ||
		key.GPUSKU == "" || key.GPUCount <= 0 || key.Precision == "" || key.TensorParallelism <= 0 ||
		key.MaxContextBucket <= 0 || key.DriverCUDAFingerprint == "" || key.ScenarioDigest == "" {
		return fmt.Errorf("%w: profile_key is incomplete", ErrInvalidReport)
	}
	if key.Backend != BackendVLLM && key.Backend != BackendTensorRTLLM {
		return fmt.Errorf("%w: unsupported profile backend %q", ErrInvalidReport, key.Backend)
	}
	if !isLowerHex(key.ScenarioDigest, 64) {
		return fmt.Errorf("%w: scenario_digest must be a lowercase SHA-256 digest", ErrInvalidReport)
	}
	return nil
}

func isLowerHex(value string, size int) bool {
	if len(value) != size {
		return false
	}
	for _, character := range value {
		if (character < '0' || character > '9') && (character < 'a' || character > 'f') {
			return false
		}
	}
	return true
}

func validateMeasurements(result Measurements) error {
	values := []float64{
		result.TTFTP50MS, result.TTFTP95MS, result.TTFTP99MS,
		result.TPOTP50MS, result.TPOTP95MS, result.TPOTP99MS,
		result.E2EP95MS, result.QueueP95MS, result.InputTokensPerSecond,
		result.OutputTokensPerSecond, result.GPUHours, result.CostPerMillionInput,
		result.CostPerMillionOutput, result.CostPerSuccessfulRequest,
		result.SLOAttainmentPercent,
	}
	for _, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return fmt.Errorf("%w: measurements must be finite and non-negative", ErrInvalidReport)
		}
	}
	if result.TTFTP50MS > result.TTFTP95MS || result.TTFTP95MS > result.TTFTP99MS ||
		result.TPOTP50MS > result.TPOTP95MS || result.TPOTP95MS > result.TPOTP99MS ||
		result.SLOAttainmentPercent > 100 {
		return fmt.Errorf("%w: measurement percentiles or SLO attainment are inconsistent", ErrInvalidReport)
	}
	return nil
}
