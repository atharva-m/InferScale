package postgres

import (
	"errors"
	"strings"
	"testing"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/benchmark"
)

func TestValidateMeasuredProfileMatchesImmutableRevision(t *testing.T) {
	spec := platformv1alpha1.InferenceDeploymentSpec{
		Model:       platformv1alpha1.ModelSpec{URI: "hf://Qwen/Qwen3", Revision: "aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"},
		Runtime:     platformv1alpha1.RuntimeSpec{Backend: platformv1alpha1.RuntimeBackendVLLM, Precision: "bf16", Quantization: "none", TensorParallelism: 1, MaxModelLen: 8192},
		Accelerator: platformv1alpha1.AcceleratorSpec{Type: "RTX_5090", Count: 1},
	}
	key := benchmark.ProfileKey{
		ModelURI: spec.Model.URI, ModelRevision: spec.Model.Revision,
		Backend: benchmark.BackendVLLM, BackendVersion: "0.10.0",
		RuntimeImageDigest: "sha256:runtime", GPUSKU: spec.Accelerator.Type,
		GPUCount: 1, Precision: "bf16", Quantization: "none", TensorParallelism: 1,
		MaxContextBucket: 8192, DriverCUDAFingerprint: "driver", ScenarioDigest: strings.Repeat("a", 64),
	}
	if err := validateMeasuredProfile(key, spec, "vllm", "vllm", "sha256:runtime"); err != nil {
		t.Fatal(err)
	}
	key.GPUCount = 2
	if err := validateMeasuredProfile(key, spec, "vllm", "vllm", "sha256:runtime"); !errors.Is(err, benchmark.ErrInvalidReport) {
		t.Fatalf("mismatch error = %v", err)
	}
}
