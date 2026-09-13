package admission

import (
	"context"
	"errors"
	"strings"
	"time"

	platformv1alpha1 "github.com/inferscale/inferscale/api/platform/v1alpha1"
	"github.com/inferscale/inferscale/internal/auth"
	"github.com/inferscale/inferscale/internal/benchmark"
	"github.com/inferscale/inferscale/internal/deployment"
	"github.com/inferscale/inferscale/internal/storage/valkey"
	"github.com/inferscale/inferscale/internal/tenant"
)

type benchmarkRunLookup interface {
	GetRun(context.Context, string) (benchmark.Run, error)
}

// RunScopedAuthenticator accepts ordinary tenant API keys and the HMAC token
// issued to one benchmark Job. A benchmark token is useful only while its run
// is active and is constrained to that run's deployment; it cannot be reused
// as a management credential or against another deployment.
type RunScopedAuthenticator struct {
	Primary Authenticator
	Tokens  *benchmark.CallbackTokens
	Runs    benchmarkRunLookup
}

func (a RunScopedAuthenticator) Authenticate(ctx context.Context, bearer string) (Principal, error) {
	if a.Primary != nil {
		principal, err := a.Primary.Authenticate(ctx, bearer)
		if err == nil {
			return principal, nil
		}
		if !strings.HasPrefix(bearer, "v1.") {
			return Principal{}, err
		}
	}
	if a.Tokens == nil || a.Runs == nil {
		return Principal{}, ErrUnauthenticated
	}
	runID, err := a.Tokens.Parse(bearer)
	if err != nil {
		return Principal{}, ErrUnauthenticated
	}
	run, err := a.Runs.GetRun(ctx, runID)
	if errors.Is(err, benchmark.ErrRunNotFound) {
		return Principal{}, ErrUnauthenticated
	}
	if err != nil {
		return Principal{}, ErrUnavailable
	}
	if run.State != benchmark.RunRunning || run.TenantID == "" || run.DeploymentID == "" || run.RevisionID == "" ||
		!supportedBenchmarkRoutingPolicy(run.Execution.RoutingPolicy) {
		return Principal{}, ErrUnauthenticated
	}
	return Principal{
		TenantID:                run.TenantID,
		KeyID:                   "benchmark:" + run.ID,
		AuthorizedDeployment:    run.DeploymentID,
		AuthorizedRevision:      run.RevisionID,
		AuthorizedRoutingPolicy: run.Execution.RoutingPolicy,
		Scopes:                  map[string]struct{}{auth.ScopeInference: {}},
	}, nil
}

type AuthServiceAdapter struct {
	Service *auth.Service
}

func (a AuthServiceAdapter) Authenticate(ctx context.Context, bearer string) (Principal, error) {
	if a.Service == nil {
		return Principal{}, ErrUnavailable
	}
	value, err := a.Service.Authenticate(ctx, bearer)
	if err != nil {
		if errors.Is(err, auth.ErrUnauthenticated) || errors.Is(err, auth.ErrForbidden) {
			return Principal{}, ErrUnauthenticated
		}
		return Principal{}, ErrUnavailable
	}
	scopes := make(map[string]struct{}, len(value.Scopes))
	for _, scope := range value.Scopes {
		scopes[scope] = struct{}{}
	}
	return Principal{
		TenantID: value.TenantID, TenantSlug: value.TenantSlug,
		KeyID: value.APIKeyID, Scopes: scopes,
	}, nil
}

type deploymentLookup interface {
	Get(context.Context, string, string) (*deployment.Deployment, error)
}

type tenantLookup interface {
	Get(context.Context, string) (*tenant.Tenant, error)
}

type PolicyRepository struct {
	Deployments deploymentLookup
	Tenants     tenantLookup
}

func (r PolicyRepository) AuthorizeDeployment(ctx context.Context, tenantID, deploymentID string) (DeploymentPolicy, error) {
	if r.Deployments == nil || r.Tenants == nil {
		return DeploymentPolicy{}, ErrUnavailable
	}
	value, err := r.Deployments.Get(ctx, tenantID, deploymentID)
	if err != nil {
		if errors.Is(err, deployment.ErrNotFound) {
			return DeploymentPolicy{}, ErrNotFound
		}
		return DeploymentPolicy{}, ErrUnavailable
	}
	tenantValue, err := r.Tenants.Get(ctx, tenantID)
	if err != nil {
		if errors.Is(err, tenant.ErrNotFound) {
			return DeploymentPolicy{}, ErrNotFound
		}
		return DeploymentPolicy{}, ErrUnavailable
	}
	// Suspension is a tenant-wide admission boundary, including credentials
	// issued to an already-running benchmark Job. Checking it here prevents
	// run-scoped tokens from bypassing the normal API-key suspension check.
	if tenantValue.SuspendedAt != nil {
		return DeploymentPolicy{}, ErrNotFound
	}
	policy := DeploymentPolicy{
		ID: value.ID, Name: value.Name, TenantID: value.TenantID,
		Phase: string(value.ObservedStatus.Phase), PriorityClass: value.Spec.Admission.PriorityClass,
		RatePerMinute:       int64(tenantValue.Quota.RequestsPerMinute),
		InferenceActive:     inferenceActive(value),
		StableRevisionID:    value.StableRevisionID,
		CandidateRevisionID: value.CandidateRevisionID,
		RoutingPolicy:       string(value.Spec.Routing.Policy),
	}
	if value.Spec.SLO != nil {
		policy.TTFTTargetMS = value.Spec.SLO.TTFT.TargetMS
		policy.TPOTTargetMS = value.Spec.SLO.TPOT.TargetMS
	}
	return policy, nil
}

func supportedBenchmarkRoutingPolicy(value string) bool {
	switch value {
	case "round-robin", "load-aware", "prefix-aware":
		return true
	default:
		return false
	}
}

func inferenceActive(value *deployment.Deployment) bool {
	if value == nil || value.DeletedAt != nil {
		return false
	}
	switch value.ObservedStatus.Phase {
	case platformv1alpha1.DeploymentPhaseReady, platformv1alpha1.DeploymentPhaseUpdating, platformv1alpha1.DeploymentPhaseDegraded:
		return value.ObservedStatus.Revision.Stable != ""
	default:
		return false
	}
}

type ValkeyRateLimiter struct {
	Client *valkey.Client
}

func (l ValkeyRateLimiter) Allow(ctx context.Context, tenantID string, limitPerMinute int64) (RateLimitDecision, error) {
	if l.Client == nil {
		return RateLimitDecision{}, ErrUnavailable
	}
	decision, err := l.Client.AllowRate(ctx, tenantID, limitPerMinute, time.Minute)
	if err != nil {
		return RateLimitDecision{}, ErrUnavailable
	}
	return RateLimitDecision{Allowed: decision.Allowed, RetryAfter: decision.RetryAfter}, nil
}
