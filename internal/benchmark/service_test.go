package benchmark

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"
)

type resultRepository struct {
	run       Run
	profile   *RuntimeProfile
	result    *Measurements
	failed    string
	completed int
}

func (r *resultRepository) CreateRun(_ context.Context, run Run) error  { r.run = run; return nil }
func (r *resultRepository) GetRun(context.Context, string) (Run, error) { return r.run, nil }
func (r *resultRepository) ListRuns(context.Context, string, string, string, int) ([]Run, string, error) {
	return []Run{r.run}, "", nil
}
func (r *resultRepository) CompleteRun(_ context.Context, _ string, result Measurements, provenance map[string]any, artifactURI string, profile *RuntimeProfile, at time.Time) (Run, error) {
	if r.run.State != RunRunning {
		return Run{}, ErrInvalidTransition
	}
	r.completed++
	r.result, r.profile = &result, profile
	r.run.State, r.run.Result, r.run.Provenance, r.run.ArtifactURI, r.run.CompletedAt = RunSucceeded, &result, provenance, artifactURI, &at
	return r.run, nil
}
func (r *resultRepository) MarkRunFailed(_ context.Context, _ string, reason string) error {
	if r.run.State != RunRunning {
		return ErrInvalidTransition
	}
	r.failed, r.run.State, r.run.Error = reason, RunFailed, reason
	return nil
}
func (*resultRepository) ApproveProfile(context.Context, string, string, time.Time) error { return nil }

func validRunningRun() Run {
	configuration := json.RawMessage(`{
		"schema_version":1,
		"measurement_tool":"guidellm-0.7.0","publishable":true,
		"workload":{"input_tokens":128,"output_tokens":32,"concurrency":1,"requests":4,"dataset":"synthetic","seed":7},
		"cache_state":"process-warm","routing":{"policy":"round-robin"},"experiment":{}
	}`)
	return Run{
		ID: "run", State: RunRunning, Namespace: "tenant-a", ExecutionConfiguration: configuration,
		ExecutionDigest: ConfigurationDigest(configuration),
		Serving: ServingContract{
			ModelURI: "hf://Qwen/Qwen3", ModelRevision: strings.Repeat("a", 40),
			Backend: BackendVLLM, RuntimeImageDigest: "sha256:" + strings.Repeat("b", 64),
			GPUSKU: "RTX_5090", GPUCount: 1, Precision: "bf16", Quantization: "none",
			TensorParallelism: 1, MaxContextBucket: 8192, RoutingPolicy: "round-robin",
		},
		Execution: ExecutionContract{
			Provider: "vast", BenchmarkTool: ScheduledBenchmarkTool, RoutingPolicy: "round-robin", InferenceBaseURL: "https://gateway.example",
			RuntimePodPrefix: "qwen-chat-a8f32-vllm",
			EPPService:       "qwen-chat-a8f32-epp",
			PrometheusURL:    "http://prometheus.monitoring.svc:9090", RuntimeVersion: "0.23.0",
			RuntimeImageDigest: "sha256:" + strings.Repeat("b", 64),
			RunnerImageDigest:  "sha256:" + strings.Repeat("c", 64),
			DriverVersion:      "580.10", CUDAVersion: "13.0",
			DatasetDigest:           "4e86ec4314e4c28066f3881b0ce5d3434fdbcfd47e5ef08824e9c70eb6d230e8",
			SelectionScenarioDigest: "b2baed04002d2407d56064ccbe098bc7466ed93043544640939ad84e151f93b4",
		},
	}
}

func validResultReport(run Run) ResultReport {
	fingerprint := DriverCUDAFingerprint(run.Execution.DriverVersion, run.Execution.CUDAVersion)
	return ResultReport{
		State: RunSucceeded,
		Measurements: &Measurements{
			TTFTP50MS: 1, TTFTP95MS: 2, TTFTP99MS: 3,
			TPOTP50MS: 1, TPOTP95MS: 2, TPOTP99MS: 3,
			CostPerSuccessfulRequest: 0.1, SLOAttainmentPercent: 100,
		},
		ProfileKey: &ProfileKey{
			ModelURI: run.Serving.ModelURI, ModelRevision: run.Serving.ModelRevision,
			Backend: BackendVLLM, BackendVersion: run.Execution.RuntimeVersion, RuntimeImageDigest: run.Execution.RuntimeImageDigest,
			GPUSKU: "RTX_5090", GPUCount: 1, Precision: "bf16", Quantization: "none",
			TensorParallelism: 1, MaxContextBucket: 8192,
			DriverCUDAFingerprint: fingerprint, ScenarioDigest: run.Execution.SelectionScenarioDigest,
		},
		Provenance: map[string]any{
			"timestamp_utc": "2026-08-21T12:00:00Z", "hostname": "runner",
			"kernel": "6.8.0", "platform": "Linux", "python": "3.11.13",
			"git_commit": nil, "git_dirty": nil, "nvidia_gpus": []any{"NVIDIA GeForce RTX 5090"},
			"provider": run.Execution.Provider, "runtime_version": run.Execution.RuntimeVersion,
			"container_image_digest":  run.Execution.RuntimeImageDigest,
			"runtime_image_digest":    run.Execution.RuntimeImageDigest,
			"runner_image_digest":     run.Execution.RunnerImageDigest,
			"nvidia_driver_version":   run.Execution.DriverVersion,
			"cuda_version":            run.Execution.CUDAVersion,
			"driver_cuda_fingerprint": fingerprint,
			"benchmark_tool":          ScheduledBenchmarkTool,
			"gpu_hourly_price":        float64(0),
			"telemetry_evidence": map[string]any{
				"valid": true,
				"required_queries": []any{
					"gpu_memory_used_bytes", "gpu_power_watts", "gpu_utilization", "kv_cache_utilization",
					"queue_p95_seconds", "running_requests", "waiting_requests",
				},
				"expected_gpu_count": float64(1), "expected_gpu_sku": "RTX_5090",
				"observed_gpus": []any{map[string]any{
					"uuid": "GPU-0001", "model": "NVIDIA GeForce RTX 5090",
					"pod": "qwen-chat-a8f32-vllm-abcde", "namespace": "tenant-a",
				}},
			},
		},
		ScenarioConfigurationDigest: run.ExecutionDigest,
		ArtifactURI:                 "s3://results/run.json",
	}
}

func TestIngestResultAppendsIneligibleProfile(t *testing.T) {
	run := validRunningRun()
	repository := &resultRepository{run: run}
	service := NewService(repository, func() string { return "profile" })
	completed, err := service.IngestResult(context.Background(), "run", validResultReport(run))
	if err != nil {
		t.Fatal(err)
	}
	if completed.State != RunSucceeded || repository.completed != 1 {
		t.Fatalf("completed run = %#v", completed)
	}
	if repository.profile == nil || repository.profile.ID != "profile" || repository.profile.Eligible || repository.profile.BenchmarkRunID != "run" {
		t.Fatalf("profile = %#v", repository.profile)
	}
}

func TestIngestResultRejectsInvalidMeasurements(t *testing.T) {
	run := validRunningRun()
	repository := &resultRepository{run: run}
	service := NewService(repository, nil)
	report := validResultReport(run)
	report.Measurements.TTFTP95MS = -1
	if _, err := service.IngestResult(context.Background(), "run", report); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("error = %v", err)
	}
	if repository.completed != 0 {
		t.Fatal("invalid result was persisted")
	}
}

func TestIngestResultRejectsCallerSuppliedProvenanceAndScenario(t *testing.T) {
	run := validRunningRun()
	repository := &resultRepository{run: run}
	service := NewService(repository, nil)
	report := validResultReport(run)
	report.Provenance["provider"] = "caller-controlled"
	if _, err := service.IngestResult(context.Background(), "run", report); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("provenance error = %v", err)
	}
	report = validResultReport(run)
	report.ScenarioConfigurationDigest = strings.Repeat("e", 64)
	if _, err := service.IngestResult(context.Background(), "run", report); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("scenario error = %v", err)
	}
	report = validResultReport(run)
	report.Provenance["prompt"] = "must never persist"
	if _, err := service.IngestResult(context.Background(), "run", report); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("unexpected provenance field error = %v", err)
	}
}

func TestIngestResultRejectsRoutingPolicyChangedAfterScheduling(t *testing.T) {
	run := validRunningRun()
	report := validResultReport(run)
	run.Serving.RoutingPolicy = "prefix-aware"
	repository := &resultRepository{run: run}
	service := NewService(repository, nil)
	if _, err := service.IngestResult(context.Background(), run.ID, report); !errors.Is(err, ErrInvalidReport) || !strings.Contains(err.Error(), "routing policy") {
		t.Fatalf("routing drift callback error = %v", err)
	}
	if repository.completed != 0 {
		t.Fatal("routing-contaminated result was persisted")
	}
}

func TestIngestResultRejectsMissingOrMismatchedTelemetryEvidence(t *testing.T) {
	run := validRunningRun()
	repository := &resultRepository{run: run}
	service := NewService(repository, nil)
	report := validResultReport(run)
	delete(report.Provenance, "telemetry_evidence")
	if _, err := service.IngestResult(context.Background(), "run", report); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("missing telemetry evidence error = %v", err)
	}

	report = validResultReport(run)
	evidence := report.Provenance["telemetry_evidence"].(map[string]any)
	evidence["expected_gpu_sku"] = "A100"
	if _, err := service.IngestResult(context.Background(), "run", report); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("mismatched GPU evidence error = %v", err)
	}
}

func TestIngestFailureRequiresReason(t *testing.T) {
	repository := &resultRepository{run: Run{ID: "run", State: RunRunning}}
	service := NewService(repository, nil)
	if _, err := service.IngestResult(context.Background(), "run", ResultReport{State: RunFailed}); !errors.Is(err, ErrInvalidReport) {
		t.Fatalf("error = %v", err)
	}
	if _, err := service.IngestResult(context.Background(), "run", ResultReport{State: RunFailed, Error: "runner exited"}); err != nil {
		t.Fatal(err)
	}
	if repository.failed != "runner exited" {
		t.Fatalf("failure = %q", repository.failed)
	}
}
