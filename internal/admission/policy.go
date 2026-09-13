package admission

import (
	"context"
	"time"
)

type Principal struct {
	TenantID                string
	TenantSlug              string
	KeyID                   string
	AuthorizedDeployment    string
	AuthorizedRevision      string
	AuthorizedRoutingPolicy string
	Scopes                  map[string]struct{}
}

func (p Principal) HasScope(scope string) bool {
	_, ok := p.Scopes[scope]
	return ok
}

type DeploymentPolicy struct {
	ID                  string
	Name                string
	TenantID            string
	Phase               string
	PriorityClass       string
	RatePerMinute       int64
	TTFTTargetMS        int64
	TPOTTargetMS        int64
	InferenceActive     bool
	StableRevisionID    string
	CandidateRevisionID string
	RoutingPolicy       string
}

type Authenticator interface {
	Authenticate(ctx context.Context, bearer string) (Principal, error)
}

type DeploymentAuthorizer interface {
	AuthorizeDeployment(ctx context.Context, tenantID, deploymentID string) (DeploymentPolicy, error)
}

type RateLimitDecision struct {
	Allowed    bool
	RetryAfter time.Duration
}

type RateLimiter interface {
	Allow(ctx context.Context, tenantID string, limitPerMinute int64) (RateLimitDecision, error)
}
