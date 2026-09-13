package deployment

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/benchmark"
	deploymentdomain "github.com/inferscale/inferscale/internal/deployment"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
)

type resolutionMemoryStore struct {
	resolution Resolution
	found      bool
	freezes    int
}

func (s *resolutionMemoryStore) LoadResolution(context.Context, string, string) (Resolution, bool, error) {
	return s.resolution, s.found, nil
}

func (s *resolutionMemoryStore) FreezeResolution(_ context.Context, _, _ string, resolution Resolution) (Resolution, error) {
	s.freezes++
	if !s.found {
		s.resolution, s.found = resolution, true
	}
	return s.resolution, nil
}

type profileMemoryRepository struct {
	profiles []deploymentdomain.RuntimeProfile
}

type staticProfileSource struct {
	selection ProfileSelection
}

func (s staticProfileSource) Selection(context.Context, platformruntime.Spec) (ProfileSelection, error) {
	return s.selection, nil
}

func (r profileMemoryRepository) ListEligibleRuntimeProfiles(context.Context, deploymentdomain.RuntimeProfileFilter) ([]deploymentdomain.RuntimeProfile, error) {
	return r.profiles, nil
}

func TestBackendResolutionIsFrozenOnce(t *testing.T) {
	t.Parallel()
	store := &resolutionMemoryStore{}
	resolver := MeasuredBackendResolver{
		Store: store,
		ExpectedRuntimes: map[benchmark.Backend]benchmark.RuntimeIdentity{
			benchmark.BackendVLLM: {BackendVersion: "0.23.0", RuntimeImageDigest: "sha256:v"},
		},
	}
	spec := platformruntime.Spec{RequestedBackend: platformruntime.BackendAuto}
	first, err := resolver.Resolve(context.Background(), RevisionIdentity{DeploymentID: "dep", RevisionID: "rev"}, spec)
	if err != nil {
		t.Fatal(err)
	}
	resolver.ExpectedRuntimes[benchmark.BackendVLLM] = benchmark.RuntimeIdentity{BackendVersion: "new", RuntimeImageDigest: "sha256:new"}
	second, err := resolver.Resolve(context.Background(), RevisionIdentity{DeploymentID: "dep", RevisionID: "rev"}, spec)
	if err != nil {
		t.Fatal(err)
	}
	if first != second || second.RuntimeImageDigest != "sha256:v" || store.freezes != 1 {
		t.Fatalf("first=%#v second=%#v freezes=%d", first, second, store.freezes)
	}
}

func TestApprovedProfileSourcePreservesExactSelectionKeys(t *testing.T) {
	t.Parallel()
	measurements, _ := json.Marshal(benchmark.Measurements{CostPerSuccessfulRequest: 1})
	profile := deploymentdomain.RuntimeProfile{
		ID: "profile", ModelURI: "hf://Qwen/Qwen3-8B", ModelRevision: "commit",
		Backend: platformv1alpha1.RuntimeBackendTensorRT, BackendVersion: "1.0.0",
		RuntimeImageDigest: "sha256:t", GPUSKU: "RTX_5090", GPUCount: 1,
		Precision: "bf16", Quantization: "none", TensorParallelism: 1,
		MaxContextBucket: 8192, DriverCUDAFingerprint: "driver", ScenarioDigest: "scenario",
		Metrics: measurements, CostPerSuccessfulRequest: 1, Eligible: true,
		MeasuredAt: time.Unix(1, 0),
	}
	source := ApprovedProfileSource{
		Repository:            profileMemoryRepository{profiles: []deploymentdomain.RuntimeProfile{profile}},
		DriverCUDAFingerprint: "driver", SelectionScenarioDigest: "scenario",
		ExpectedRuntimes: map[benchmark.Backend]benchmark.RuntimeIdentity{
			benchmark.BackendVLLM:        {BackendVersion: "0.23.0", RuntimeImageDigest: "sha256:v"},
			benchmark.BackendTensorRTLLM: {BackendVersion: "1.0.0", RuntimeImageDigest: "sha256:t"},
		},
	}
	input, err := source.Selection(context.Background(), platformruntime.Spec{
		ModelURI: profile.ModelURI, ModelRevision: profile.ModelRevision,
		AcceleratorType: profile.GPUSKU, AcceleratorCount: 1,
		Precision: "bf16", Quantization: "none", TensorParallel: 1, MaxModelLen: 8192,
	})
	if err != nil {
		t.Fatal(err)
	}
	selection := benchmark.SelectBackend(input.Request, input.Profiles)
	if selection.ProfileID != profile.ID || selection.Backend != benchmark.BackendTensorRTLLM {
		t.Fatalf("selection=%#v", selection)
	}
	input.Request.ExpectedRuntimes[benchmark.BackendTensorRTLLM] = benchmark.RuntimeIdentity{
		BackendVersion: "1.0.0", RuntimeImageDigest: "sha256:other",
	}
	selection = benchmark.SelectBackend(input.Request, input.Profiles)
	if selection.Status != benchmark.SelectionUnbenchmarked || selection.Backend != benchmark.BackendVLLM {
		t.Fatalf("stale runtime profile was selected: %#v", selection)
	}
}

func TestAutoSelectionFiltersUnsupportedTensorRTProfilesBeforeRanking(t *testing.T) {
	t.Parallel()
	for _, test := range []struct {
		name string
		spec platformruntime.Spec
		want platformruntime.Backend
	}{
		{
			name: "prefix-aware",
			spec: platformruntime.Spec{RequestedBackend: platformruntime.BackendAuto, RoutingPolicy: "prefix-aware", PrefixCaching: true, Precision: "bf16", Quantization: "none"},
			want: platformruntime.BackendVLLM,
		},
		{
			name: "AWQ",
			spec: platformruntime.Spec{RequestedBackend: platformruntime.BackendAuto, RoutingPolicy: "load-aware", PrefixCaching: true, Precision: "bf16", Quantization: "awq"},
			want: platformruntime.BackendVLLM,
		},
		{
			name: "fp8",
			spec: platformruntime.Spec{RequestedBackend: platformruntime.BackendAuto, RoutingPolicy: "load-aware", PrefixCaching: true, Precision: "fp8", Quantization: "none"},
			want: platformruntime.BackendVLLM,
		},
		{
			name: "supported",
			spec: platformruntime.Spec{RequestedBackend: platformruntime.BackendAuto, RoutingPolicy: "load-aware", PrefixCaching: true, Precision: "bf16", Quantization: "none"},
			want: platformruntime.BackendTRTLLM,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			base := benchmark.ProfileKey{
				ModelURI: "hf://Qwen/Qwen3-8B", ModelRevision: "commit", GPUSKU: "RTX_5090", GPUCount: 1,
				Precision: test.spec.Precision, Quantization: test.spec.Quantization, TensorParallelism: 1,
				MaxContextBucket: 8192, DriverCUDAFingerprint: "driver", ScenarioDigest: "scenario",
			}
			vllmKey, trtKey := base, base
			vllmKey.Backend, vllmKey.BackendVersion, vllmKey.RuntimeImageDigest = benchmark.BackendVLLM, "0.23.0", "sha256:v"
			trtKey.Backend, trtKey.BackendVersion, trtKey.RuntimeImageDigest = benchmark.BackendTensorRTLLM, "1.0.0", "sha256:t"
			request := benchmark.SelectionRequest{
				KeyWithoutBackend: base,
				ExpectedRuntimes: map[benchmark.Backend]benchmark.RuntimeIdentity{
					benchmark.BackendVLLM:        {BackendVersion: "0.23.0", RuntimeImageDigest: "sha256:v"},
					benchmark.BackendTensorRTLLM: {BackendVersion: "1.0.0", RuntimeImageDigest: "sha256:t"},
				},
			}
			resolver := MeasuredBackendResolver{
				Source: staticProfileSource{selection: ProfileSelection{
					Request: request,
					Profiles: []benchmark.RuntimeProfile{
						{ID: "v", Key: vllmKey, Eligible: true, Measurements: benchmark.Measurements{CostPerSuccessfulRequest: 2}},
						{ID: "t", Key: trtKey, Eligible: true, Measurements: benchmark.Measurements{CostPerSuccessfulRequest: 1}},
					},
				}},
				AutoEnabled: true, TensorRTEnabled: true,
			}
			resolution, err := resolver.Resolve(context.Background(), RevisionIdentity{}, test.spec)
			if err != nil {
				t.Fatal(err)
			}
			if resolution.Backend != test.want {
				t.Fatalf("selected backend = %q, want %q", resolution.Backend, test.want)
			}
		})
	}
}

func TestExplicitTensorRTResolutionRejectsUnsupportedContract(t *testing.T) {
	t.Parallel()
	resolver := MeasuredBackendResolver{TensorRTEnabled: true}
	for _, spec := range []platformruntime.Spec{
		{RequestedBackend: platformruntime.BackendTRTLLM, RoutingPolicy: "prefix-aware", Precision: "bf16", Quantization: "none"},
		{RequestedBackend: platformruntime.BackendTRTLLM, RoutingPolicy: "load-aware", Precision: "bf16", Quantization: "awq"},
		{RequestedBackend: platformruntime.BackendTRTLLM, RoutingPolicy: "load-aware", Precision: "fp8", Quantization: "none"},
	} {
		if _, err := resolver.Resolve(context.Background(), RevisionIdentity{}, spec); err == nil {
			t.Fatalf("unsupported explicit TensorRT contract was resolved: %#v", spec)
		}
	}
}
