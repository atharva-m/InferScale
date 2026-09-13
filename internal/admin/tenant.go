package admin

import (
	"context"
	"fmt"

	"github.com/inferscale/inferscale/internal/tenant"
)

// TenantAdmin exposes the operator-only tenant lifecycle used by
// inferscalectl. Tenant management is deliberately not part of the public API.
type TenantAdmin struct {
	Service    *tenant.Service
	Repository tenant.Repository
}

func (a TenantAdmin) Create(ctx context.Context, slug, name string, quota tenant.Quota) (*tenant.Tenant, error) {
	if a.Service == nil {
		return nil, fmt.Errorf("tenant service is not configured")
	}
	return a.Service.Create(ctx, slug, name, quota)
}

func (a TenantAdmin) List(ctx context.Context, limit, offset int) ([]tenant.Tenant, error) {
	if a.Repository == nil {
		return nil, fmt.Errorf("tenant repository is not configured")
	}
	if limit <= 0 || limit > 200 {
		limit = 50
	}
	if offset < 0 {
		offset = 0
	}
	return a.Repository.List(ctx, limit, offset)
}

func (a TenantAdmin) Suspend(ctx context.Context, id string) error {
	if a.Service == nil {
		return fmt.Errorf("tenant service is not configured")
	}
	return a.Service.Suspend(ctx, id)
}

func (a TenantAdmin) Resume(ctx context.Context, id string) error {
	if a.Service == nil {
		return fmt.Errorf("tenant service is not configured")
	}
	return a.Service.Resume(ctx, id)
}

func (a TenantAdmin) Provision(ctx context.Context, id string) error {
	if a.Service == nil {
		return fmt.Errorf("tenant service is not configured")
	}
	return a.Service.ProvisionNamespace(ctx, id)
}
