package deployment

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/inferscale/inferscale/internal/benchmark"
	deploymentdomain "github.com/inferscale/inferscale/internal/deployment"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
)

type ApprovedProfileRepository interface {
	ListEligibleRuntimeProfiles(context.Context, deploymentdomain.RuntimeProfileFilter) ([]deploymentdomain.RuntimeProfile, error)
}

// ApprovedProfileSource adapts the append-only PostgreSQL profile registry to
// backend selection. All fields that identify the measured environment are
// supplied explicitly and compared exactly by benchmark.SelectBackend.
type ApprovedProfileSource struct {
	Repository              ApprovedProfileRepository
	DriverCUDAFingerprint   string
	SelectionScenarioDigest string
	ExpectedRuntimes        map[benchmark.Backend]benchmark.RuntimeIdentity
}

func (s ApprovedProfileSource) Selection(ctx context.Context, spec platformruntime.Spec) (ProfileSelection, error) {
	if s.Repository == nil {
		return ProfileSelection{}, fmt.Errorf("approved runtime-profile repository is required")
	}
	if strings.TrimSpace(s.DriverCUDAFingerprint) == "" || strings.TrimSpace(s.SelectionScenarioDigest) == "" {
		return ProfileSelection{}, fmt.Errorf("driver/CUDA fingerprint and selection-scenario digest are required for exact profile matching")
	}
	if len(s.ExpectedRuntimes) == 0 {
		return ProfileSelection{}, fmt.Errorf("at least one exact backend runtime identity is required")
	}
	for backend, identity := range s.ExpectedRuntimes {
		if strings.TrimSpace(identity.BackendVersion) == "" || strings.TrimSpace(identity.RuntimeImageDigest) == "" {
			return ProfileSelection{}, fmt.Errorf("runtime version and image digest are required for backend %q", backend)
		}
	}

	values, err := s.Repository.ListEligibleRuntimeProfiles(ctx, deploymentdomain.RuntimeProfileFilter{
		ModelURI: spec.ModelURI, ModelRevision: spec.ModelRevision,
		GPUSKU: spec.AcceleratorType, GPUCount: spec.AcceleratorCount,
		Precision: spec.Precision, Quantization: spec.Quantization,
		TensorParallelism: spec.TensorParallel, MaxContextBucket: spec.MaxModelLen,
	})
	if err != nil {
		return ProfileSelection{}, err
	}
	profiles := make([]benchmark.RuntimeProfile, 0, len(values))
	for _, value := range values {
		backend := benchmark.Backend(value.Backend)
		if backend != benchmark.BackendVLLM && backend != benchmark.BackendTensorRTLLM {
			continue
		}
		var measurements benchmark.Measurements
		if err := json.Unmarshal(value.Metrics, &measurements); err != nil {
			return ProfileSelection{}, fmt.Errorf("decode measurements for profile %s: %w", value.ID, err)
		}
		measurements.CostPerSuccessfulRequest = value.CostPerSuccessfulRequest
		profiles = append(profiles, benchmark.RuntimeProfile{
			ID: value.ID,
			Key: benchmark.ProfileKey{
				ModelURI: value.ModelURI, ModelRevision: value.ModelRevision,
				Backend: backend, BackendVersion: value.BackendVersion,
				RuntimeImageDigest: value.RuntimeImageDigest, GPUSKU: value.GPUSKU,
				GPUCount: value.GPUCount, Precision: value.Precision,
				Quantization: value.Quantization, TensorParallelism: value.TensorParallelism,
				MaxContextBucket:      value.MaxContextBucket,
				DriverCUDAFingerprint: value.DriverCUDAFingerprint,
				ScenarioDigest:        value.ScenarioDigest,
			},
			Measurements: measurements, BenchmarkRunID: value.SourceBenchmarkRunID,
			Eligible: value.Eligible, ApprovedBy: value.ApprovedBy,
			ApprovedAt: value.ApprovedAt, MeasuredAt: value.MeasuredAt,
		})
	}
	return ProfileSelection{
		Profiles: profiles,
		Request: benchmark.SelectionRequest{
			KeyWithoutBackend: benchmark.ProfileKey{
				ModelURI: spec.ModelURI, ModelRevision: spec.ModelRevision,
				GPUSKU: spec.AcceleratorType, GPUCount: spec.AcceleratorCount,
				Precision: spec.Precision, Quantization: spec.Quantization,
				TensorParallelism: spec.TensorParallel, MaxContextBucket: spec.MaxModelLen,
				DriverCUDAFingerprint: s.DriverCUDAFingerprint,
				ScenarioDigest:        s.SelectionScenarioDigest,
			},
			ExpectedRuntimes: s.ExpectedRuntimes,
		},
	}, nil
}
