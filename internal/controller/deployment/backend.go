package deployment

import (
	"context"
	"fmt"
	"strings"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/benchmark"
	platformruntime "github.com/inferscale/inferscale/internal/runtime"
)

type Resolution = platformruntime.BackendResolution

type RevisionIdentity struct {
	DeploymentID string
	RevisionID   string
}

type BackendResolver interface {
	Resolve(context.Context, RevisionIdentity, platformruntime.Spec) (Resolution, error)
}

// ResolutionStore is the compare-and-set boundary that freezes one backend
// decision into an immutable PostgreSQL deployment revision.
type ResolutionStore interface {
	LoadResolution(context.Context, string, string) (Resolution, bool, error)
	FreezeResolution(context.Context, string, string, Resolution) (Resolution, error)
}

type ProfileSelection struct {
	Profiles []benchmark.RuntimeProfile
	Request  benchmark.SelectionRequest
}

// ProfileSource returns approved profiles together with all exact environment
// identities used to compare them. It must never wildcard runtime, driver/CUDA,
// or standardized-scenario fields.
type ProfileSource interface {
	Selection(context.Context, platformruntime.Spec) (ProfileSelection, error)
}

type MeasuredBackendResolver struct {
	Source           ProfileSource
	Store            ResolutionStore
	ExpectedRuntimes map[benchmark.Backend]benchmark.RuntimeIdentity
	AutoEnabled      bool
	TensorRTEnabled  bool
}

func (r MeasuredBackendResolver) Resolve(ctx context.Context, identity RevisionIdentity, spec platformruntime.Spec) (Resolution, error) {
	if r.Store != nil {
		if strings.TrimSpace(identity.DeploymentID) == "" || strings.TrimSpace(identity.RevisionID) == "" {
			return Resolution{}, fmt.Errorf("deployment and immutable revision IDs are required to freeze backend selection")
		}
		frozen, found, err := r.Store.LoadResolution(ctx, identity.DeploymentID, identity.RevisionID)
		if err != nil {
			return Resolution{}, fmt.Errorf("load frozen backend resolution: %w", err)
		}
		if found {
			if spec.RequestedBackend != platformruntime.BackendAuto && frozen.Backend != spec.RequestedBackend {
				return Resolution{}, fmt.Errorf("frozen backend %q conflicts with requested backend %q", frozen.Backend, spec.RequestedBackend)
			}
			return frozen, nil
		}
	}

	resolution, err := r.selectBackend(ctx, spec)
	if err != nil {
		return Resolution{}, err
	}
	if r.Store == nil {
		return resolution, nil
	}
	frozen, err := r.Store.FreezeResolution(ctx, identity.DeploymentID, identity.RevisionID, resolution)
	if err != nil {
		return Resolution{}, fmt.Errorf("freeze backend resolution: %w", err)
	}
	return frozen, nil
}

func (r MeasuredBackendResolver) selectBackend(ctx context.Context, spec platformruntime.Spec) (Resolution, error) {
	switch spec.RequestedBackend {
	case platformruntime.BackendVLLM:
		return Resolution{
			Backend: platformruntime.BackendVLLM, SelectionStatus: "explicit",
			RuntimeImageDigest: RuntimeImageIdentity(spec.RuntimeVersion),
		}, nil
	case platformruntime.BackendTRTLLM:
		if !r.TensorRTEnabled {
			return Resolution{}, fmt.Errorf("TensorRT-LLM feature is disabled")
		}
		if !tensorRTSupports(spec) {
			return Resolution{}, fmt.Errorf("TensorRT-LLM does not support routing policy %q, precision %q, and quantization %q in v1", spec.RoutingPolicy, spec.Precision, spec.Quantization)
		}
		return Resolution{
			Backend: platformruntime.BackendTRTLLM, SelectionStatus: "explicit",
			RuntimeImageDigest: RuntimeImageIdentity(spec.RuntimeVersion),
		}, nil
	case platformruntime.BackendAuto:
		if !r.AutoEnabled {
			identity, ok := r.ExpectedRuntimes[benchmark.BackendVLLM]
			if !ok || identity.RuntimeImageDigest == "" {
				return Resolution{}, fmt.Errorf("exact pinned vLLM runtime identity is required for backend:auto fallback")
			}
			return Resolution{
				Backend: platformruntime.BackendVLLM, SelectionStatus: benchmark.SelectionUnbenchmarked,
				RuntimeImageDigest: identity.RuntimeImageDigest,
			}, nil
		}
		if r.Source == nil {
			return Resolution{}, fmt.Errorf("backend:auto is enabled without an approved-profile source")
		}
	default:
		return Resolution{}, fmt.Errorf("unsupported requested backend %q", spec.RequestedBackend)
	}

	input, err := r.Source.Selection(ctx, spec)
	if err != nil {
		return Resolution{}, fmt.Errorf("load compatible benchmark profiles: %w", err)
	}
	if spec.TTFT != nil || spec.TPOT != nil {
		input.Request.SLO = &benchmark.SLO{}
		if spec.TTFT != nil {
			if spec.TTFT.Percentile != 95 {
				return Resolution{}, fmt.Errorf("backend:auto currently requires a P95 TTFT SLO")
			}
			input.Request.SLO.TTFTP95MS = float64(spec.TTFT.TargetMS)
		}
		if spec.TPOT != nil {
			if spec.TPOT.Percentile != 95 {
				return Resolution{}, fmt.Errorf("backend:auto currently requires a P95 TPOT SLO")
			}
			input.Request.SLO.TPOTP95MS = float64(spec.TPOT.TargetMS)
		}
	}
	// Profile compatibility is necessary but not sufficient: a measured
	// backend must also implement this revision's routing and serving contract.
	// Filter before ranking so an ineligible cheap TensorRT profile cannot win
	// and then be silently replaced after selection.
	input.Profiles = eligibleAutoProfiles(spec, input.Profiles, r.TensorRTEnabled)
	selection := benchmark.SelectBackend(input.Request, input.Profiles)
	backend := platformruntime.Backend(selection.Backend)
	identity, ok := input.Request.ExpectedRuntimes[benchmark.Backend(backend)]
	if !ok || identity.BackendVersion == "" || identity.RuntimeImageDigest == "" {
		return Resolution{}, fmt.Errorf("exact pinned runtime identity is missing for selected backend %q", backend)
	}
	return Resolution{
		Backend: backend, ProfileID: selection.ProfileID, SelectionStatus: selection.Status,
		RuntimeImageDigest: identity.RuntimeImageDigest,
	}, nil
}

func eligibleAutoProfiles(spec platformruntime.Spec, profiles []benchmark.RuntimeProfile, tensorRTEnabled bool) []benchmark.RuntimeProfile {
	eligible := make([]benchmark.RuntimeProfile, 0, len(profiles))
	for _, profile := range profiles {
		switch profile.Key.Backend {
		case benchmark.BackendVLLM:
			// vLLM prefix-aware routing requires the KV-event source, which is
			// enabled only when prefix caching is part of the serving contract.
			if spec.RoutingPolicy == string(platformv1alpha1.RoutingPolicyPrefixAware) && !spec.PrefixCaching {
				continue
			}
		case benchmark.BackendTensorRTLLM:
			if !tensorRTEnabled || !tensorRTSupports(spec) {
				continue
			}
		default:
			continue
		}
		eligible = append(eligible, profile)
	}
	return eligible
}

func tensorRTSupports(spec platformruntime.Spec) bool {
	return spec.RoutingPolicy != string(platformv1alpha1.RoutingPolicyPrefixAware) &&
		spec.Precision == "bf16" && (spec.Quantization == "" || spec.Quantization == "none")
}

func RuntimeImageIdentity(image string) string {
	if at := strings.LastIndex(image, "@sha256:"); at >= 0 {
		return image[at+1:]
	}
	return image
}

func defaultRuntimeIdentities(images platformruntime.Images, tensorRTEnabled bool) map[benchmark.Backend]benchmark.RuntimeIdentity {
	identities := map[benchmark.Backend]benchmark.RuntimeIdentity{
		benchmark.BackendVLLM: {
			BackendVersion: "unconfigured", RuntimeImageDigest: RuntimeImageIdentity(images.VLLM),
		},
	}
	if tensorRTEnabled {
		identities[benchmark.BackendTensorRTLLM] = benchmark.RuntimeIdentity{
			BackendVersion: "unconfigured", RuntimeImageDigest: RuntimeImageIdentity(images.TensorRTLLM),
		}
	}
	return identities
}
