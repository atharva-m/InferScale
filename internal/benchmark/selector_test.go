package benchmark

import "testing"

func TestSelectBackendUsesMeasuredCost(t *testing.T) {
	base := ProfileKey{ModelURI: "hf://Qwen/Qwen3-8B", ModelRevision: "abc", RuntimeImageDigest: "sha256:x", GPUSKU: "RTX_5090", GPUCount: 1, Precision: "bf16", TensorParallelism: 1, MaxContextBucket: 8192, DriverCUDAFingerprint: "driver", ScenarioDigest: "scenario"}
	vllm := base
	vllm.Backend = BackendVLLM
	trt := base
	trt.Backend = BackendTensorRTLLM
	selection := SelectBackend(SelectionRequest{KeyWithoutBackend: base}, []RuntimeProfile{
		{ID: "v", Key: vllm, Eligible: true, Measurements: Measurements{CostPerSuccessfulRequest: 2, TTFTP95MS: 10}},
		{ID: "t", Key: trt, Eligible: true, Measurements: Measurements{CostPerSuccessfulRequest: 1, TTFTP95MS: 20}},
	})
	if selection.Backend != BackendTensorRTLLM || selection.ProfileID != "t" || selection.Status != SelectionMeasured {
		t.Fatalf("selection = %#v", selection)
	}
}

func TestSelectBackendFallsBackWithoutCompatibleProfile(t *testing.T) {
	selection := SelectBackend(SelectionRequest{KeyWithoutBackend: ProfileKey{ModelRevision: "wanted"}}, []RuntimeProfile{{Eligible: true, Key: ProfileKey{ModelRevision: "other"}}})
	if selection.Backend != BackendVLLM || selection.Status != SelectionUnbenchmarked {
		t.Fatalf("selection = %#v", selection)
	}
}

func TestSelectBackendUsesPerBackendRuntimeIdentities(t *testing.T) {
	t.Parallel()
	key := ProfileKey{
		ModelURI: "hf://Qwen/Qwen3-8B", ModelRevision: "abc", GPUSKU: "RTX_5090",
		GPUCount: 1, Precision: "bf16", Quantization: "none", TensorParallelism: 1,
		MaxContextBucket: 8192, DriverCUDAFingerprint: "driver", ScenarioDigest: "scenario",
	}
	profiles := []RuntimeProfile{
		{ID: "v", Eligible: true, Key: key, Measurements: Measurements{CostPerSuccessfulRequest: 2, TTFTP95MS: 20}},
		{ID: "t", Eligible: true, Key: key, Measurements: Measurements{CostPerSuccessfulRequest: 1, TTFTP95MS: 20}},
	}
	profiles[0].Key.Backend = BackendVLLM
	profiles[0].Key.BackendVersion = "0.23.0"
	profiles[0].Key.RuntimeImageDigest = "sha256:v"
	profiles[1].Key.Backend = BackendTensorRTLLM
	profiles[1].Key.BackendVersion = "1.0.0"
	profiles[1].Key.RuntimeImageDigest = "sha256:t"

	request := SelectionRequest{
		KeyWithoutBackend: key,
		ExpectedRuntimes: map[Backend]RuntimeIdentity{
			BackendVLLM:        {BackendVersion: "0.23.0", RuntimeImageDigest: "sha256:v"},
			BackendTensorRTLLM: {BackendVersion: "1.0.0", RuntimeImageDigest: "sha256:t"},
		},
	}
	selection := SelectBackend(request, profiles)
	if selection.ProfileID != "t" || selection.Backend != BackendTensorRTLLM {
		t.Fatalf("selection = %#v, want exact TensorRT-LLM profile", selection)
	}

	profiles[1].Key.RuntimeImageDigest = "sha256:stale"
	selection = SelectBackend(request, profiles)
	if selection.ProfileID != "v" {
		t.Fatalf("selection = %#v, stale image profile must be excluded", selection)
	}
}
