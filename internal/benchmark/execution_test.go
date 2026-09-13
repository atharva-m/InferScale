package benchmark

import (
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPinnedDatasetAndSelectionDigestsMatchRunnerContract(t *testing.T) {
	for name, expected := range pinnedDatasetDigests {
		contents, err := os.ReadFile(filepath.Join("..", "..", "benchmarks", "workloads", name))
		if err != nil {
			t.Fatal(err)
		}
		actual := sha256.Sum256(contents)
		if got := fmt.Sprintf("%x", actual); got != expected {
			t.Fatalf("dataset %s digest = %s, want %s", name, got, expected)
		}
	}
	configuration := json.RawMessage(`{
		"schema_version":1,
		"measurement_tool":"guidellm-0.7.0","publishable":true,
		"workload":{"input_tokens":128,"output_tokens":32,"concurrency":1,"requests":4,"dataset":"synthetic","seed":7},
		"cache_state":"process-warm","routing":{"policy":"round-robin"},"experiment":{}
	}`)
	datasetDigest, scenarioDigest, err := selectionScenarioDigest(configuration)
	if err != nil {
		t.Fatal(err)
	}
	wantDataset := sha256.Sum256([]byte(syntheticDatasetIdentity))
	if datasetDigest != fmt.Sprintf("%x", wantDataset) || scenarioDigest != "b2baed04002d2407d56064ccbe098bc7466ed93043544640939ad84e151f93b4" {
		t.Fatalf("dataset/scenario digests = %s %s", datasetDigest, scenarioDigest)
	}
}

func TestExecutionAuthorityReplacesCallerServingAndTargetFields(t *testing.T) {
	run := Run{
		ID: "run", DeploymentID: "deployment-id", DeploymentName: "qwen-chat", ScenarioName: "standard",
		RuntimePodPrefix: "qwen-chat-a8f32-vllm",
		EPPService:       "qwen-chat-a8f32-epp",
		RentalPriceUSD:   1.25,
		Configuration: json.RawMessage(`{
			"schema_version":1,
			"name":"caller-name",
			"target":{"base_url":"https://attacker.example","deployment":"wrong"},
			"deployment":{"model":"attacker/model","backend":"tensorrt-llm"},
			"workload":{"input_tokens":128,"output_tokens":32,"concurrency":4,"requests":200,"dataset":"../workloads/smoke.jsonl","seed":7},
			"cache_state":"process-warm",
			"routing":{"policy":"load-aware"}
		}`),
		Serving: ServingContract{
			ModelURI: "hf://Qwen/Qwen3-8B", ModelRevision: strings.Repeat("a", 40),
			Backend: BackendVLLM, RuntimeImageDigest: "sha256:" + strings.Repeat("b", 64),
			GPUSKU: "RTX_5090", GPUCount: 1, Precision: "bf16", Quantization: "none",
			TensorParallelism: 1, MaxContextBucket: 8192, PrefixCaching: true,
			RoutingPolicy: "load-aware",
		},
	}
	authority := ExecutionAuthority{
		Provider: "vast", InferenceBaseURL: "https://gateway.example",
		PrometheusURL: "http://prometheus.monitoring.svc:9090",
		DriverVersion: "580.10", CUDAVersion: "13.0",
		RunnerImage:     "runner@sha256:" + strings.Repeat("c", 64),
		RuntimeVersions: map[Backend]string{BackendVLLM: "0.23.0"},
	}
	prepared, err := authority.Prepare(run)
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(prepared.ExecutionConfiguration, &document); err != nil {
		t.Fatal(err)
	}
	target := document["target"].(map[string]any)
	deployment := document["deployment"].(map[string]any)
	if target["base_url"] != "https://gateway.example" || target["deployment"] != "qwen-chat" {
		t.Fatalf("target = %#v", target)
	}
	if deployment["model"] != "Qwen/Qwen3-8B" || deployment["backend"] != "vllm" || deployment["backend_version"] != "0.23.0" {
		t.Fatalf("deployment = %#v", deployment)
	}
	if document["measurement_tool"] != ScheduledBenchmarkTool || document["publishable"] != true {
		t.Fatalf("scheduled measurement contract = %#v", document)
	}
	if prepared.Execution.RuntimePodPrefix != run.RuntimePodPrefix {
		t.Fatalf("runtime Pod prefix = %q, want %q", prepared.Execution.RuntimePodPrefix, run.RuntimePodPrefix)
	}
	if prepared.Execution.EPPService != run.EPPService {
		t.Fatalf("EPP Service = %q, want %q", prepared.Execution.EPPService, run.EPPService)
	}
	if prepared.Execution.RoutingPolicy != run.Serving.RoutingPolicy {
		t.Fatalf("routing policy = %q, want %q", prepared.Execution.RoutingPolicy, run.Serving.RoutingPolicy)
	}
	if prepared.ExecutionDigest != ConfigurationDigest(prepared.ExecutionConfiguration) {
		t.Fatal("prepared execution digest does not bind the canonical scenario")
	}

	// Once persisted, changed process configuration cannot rewrite an active
	// run; scheduler failover must reuse exactly the original snapshot.
	changed := authority
	changed.Provider = "different-provider"
	replayed, err := changed.Prepare(prepared)
	if err != nil {
		t.Fatal(err)
	}
	if replayed.ExecutionDigest != prepared.ExecutionDigest || replayed.Execution.Provider != "vast" {
		t.Fatalf("persisted execution snapshot changed: %#v", replayed.Execution)
	}
}

func TestExecutionAuthorityRejectsPersistedRoutingPolicyDrift(t *testing.T) {
	run := Run{
		ID: "run", DeploymentID: "deployment-id", DeploymentName: "qwen-chat", ScenarioName: "standard",
		RuntimePodPrefix: "qwen-chat-a8f32-vllm", EPPService: "qwen-chat-a8f32-epp",
		RentalPriceUSD: 1.25,
		Configuration: json.RawMessage(`{
			"schema_version":1,
			"workload":{"input_tokens":128,"output_tokens":32,"concurrency":1,"requests":4,"dataset":"synthetic","seed":7},
			"cache_state":"process-warm","routing":{"policy":"load-aware"}
		}`),
		Serving: ServingContract{
			ModelURI: "hf://Qwen/Qwen3-8B", ModelRevision: strings.Repeat("a", 40), Backend: BackendVLLM,
			RuntimeImageDigest: "sha256:" + strings.Repeat("b", 64), GPUSKU: "RTX_5090", GPUCount: 1,
			Precision: "bf16", Quantization: "none", TensorParallelism: 1, MaxContextBucket: 8192,
			RoutingPolicy: "load-aware",
		},
	}
	authority := ExecutionAuthority{
		Provider: "vast", InferenceBaseURL: "https://gateway.example", PrometheusURL: "http://prometheus:9090",
		DriverVersion: "580.10", CUDAVersion: "13.0", RunnerImage: "runner@sha256:" + strings.Repeat("c", 64),
		RuntimeVersions: map[Backend]string{BackendVLLM: "0.23.0"},
	}
	prepared, err := authority.Prepare(run)
	if err != nil {
		t.Fatal(err)
	}
	prepared.Serving.RoutingPolicy = "prefix-aware"
	if _, err := authority.Prepare(prepared); err == nil || !strings.Contains(err.Error(), "routing policy") {
		t.Fatalf("persisted routing drift error = %v", err)
	}
}

func TestExecutionAuthorityRejectsUnverifiedRoutingAndCacheClaims(t *testing.T) {
	configuration := `{
		"schema_version":1,
		"workload":{"input_tokens":128,"output_tokens":32,"concurrency":1,"requests":4,"dataset":"synthetic","seed":7},
		"cache_state":"process-warm","routing":{"policy":"load-aware"}
	}`
	run := Run{
		ID: "run", DeploymentID: "deployment-id", DeploymentName: "qwen-chat", ScenarioName: "standard",
		RuntimePodPrefix: "qwen-chat-a8f32-vllm", EPPService: "qwen-chat-a8f32-epp",
		RentalPriceUSD: 1.25, Configuration: json.RawMessage(configuration),
		Serving: ServingContract{
			ModelURI: "hf://Qwen/Qwen3-8B", ModelRevision: strings.Repeat("a", 40),
			Backend: BackendVLLM, RuntimeImageDigest: "sha256:" + strings.Repeat("b", 64),
			GPUSKU: "RTX_5090", GPUCount: 1, Precision: "bf16", Quantization: "none",
			TensorParallelism: 1, MaxContextBucket: 8192, RoutingPolicy: "load-aware",
		},
	}
	authority := ExecutionAuthority{
		Provider: "vast", InferenceBaseURL: "https://gateway.example",
		PrometheusURL: "http://prometheus.monitoring.svc:9090",
		DriverVersion: "580.10", CUDAVersion: "13.0",
		RunnerImage:     "runner@sha256:" + strings.Repeat("c", 64),
		RuntimeVersions: map[Backend]string{BackendVLLM: "0.23.0"},
	}

	routingMismatch := run
	routingMismatch.Configuration = json.RawMessage(strings.Replace(configuration, "load-aware", "round-robin", 1))
	if _, err := authority.Prepare(routingMismatch); err == nil || !strings.Contains(err.Error(), "routing policy") {
		t.Fatalf("routing mismatch error = %v", err)
	}
	coldClaim := run
	coldClaim.Configuration = json.RawMessage(strings.Replace(configuration, "process-warm", "remote-cold", 1))
	if _, err := authority.Prepare(coldClaim); err == nil || !strings.Contains(err.Error(), "process-warm") {
		t.Fatalf("unprepared cache-state error = %v", err)
	}
}

func TestExecutionAuthorityRejectsMutableImages(t *testing.T) {
	run := Run{
		Configuration: json.RawMessage(`{"schema_version":1,"workload":{"dataset":"../workloads/smoke.jsonl"},"routing":{}}`),
		Serving: ServingContract{
			ModelURI: "hf://Qwen/Qwen3", ModelRevision: strings.Repeat("a", 40), Backend: BackendVLLM,
			RuntimeImageDigest: "mutable", GPUSKU: "RTX_5090", GPUCount: 1,
			Precision: "bf16", TensorParallelism: 1, MaxContextBucket: 8192,
			RoutingPolicy: "round-robin",
		},
	}
	_, err := (ExecutionAuthority{
		Provider: "vast", InferenceBaseURL: "https://gateway.example",
		PrometheusURL: "http://prometheus:9090", DriverVersion: "580.10", CUDAVersion: "13.0",
		RunnerImage: "runner:latest", RuntimeVersions: map[Backend]string{BackendVLLM: "0.23.0"},
	}).Prepare(run)
	if err == nil {
		t.Fatal("mutable runtime and runner images were accepted")
	}
}
