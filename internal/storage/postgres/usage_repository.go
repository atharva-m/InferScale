package postgres

import (
	"context"
	"errors"
	"fmt"
	"math"
	"strings"
	"time"

	"github.com/inferscale/inferscale/internal/deployment"
	"github.com/inferscale/inferscale/internal/usage"
	"github.com/jackc/pgx/v5"
)

type UsageRepository struct{ store *Store }

func NewUsageRepository(store *Store) *UsageRepository { return &UsageRepository{store: store} }

// ResolveUsageDeployment resolves a Prometheus resource identity even after a
// soft deletion. Deployment names are permanently unique within a tenant,
// including after deletion, so historical usage remains attributable without
// exposing public IDs as scrape labels.
func (r *UsageRepository) ResolveUsageDeployment(ctx context.Context, tenantID, name string) (string, error) {
	var id string
	err := r.store.pool.QueryRow(ctx, `
		SELECT id FROM deployments WHERE tenant_id=$1 AND name=$2`, tenantID, name).Scan(&id)
	if errors.Is(err, pgx.ErrNoRows) {
		return "", deployment.ErrNotFound
	}
	return id, err
}

func (r *UsageRepository) UpsertHourly(ctx context.Context, aggregate usage.Hourly) error {
	if strings.TrimSpace(aggregate.TenantID) == "" || strings.TrimSpace(aggregate.DeploymentID) == "" || strings.TrimSpace(aggregate.Revision) == "" {
		return fmt.Errorf("tenant, deployment, and revision are required for hourly usage")
	}
	if aggregate.Requests < 0 || aggregate.RejectedRequests < 0 || aggregate.InputTokens < 0 || aggregate.OutputTokens < 0 ||
		aggregate.GPUSeconds < 0 || aggregate.ShadowGPUSeconds < 0 || math.IsNaN(aggregate.GPUSeconds) ||
		math.IsNaN(aggregate.ShadowGPUSeconds) || math.IsInf(aggregate.GPUSeconds, 0) || math.IsInf(aggregate.ShadowGPUSeconds, 0) {
		return fmt.Errorf("hourly usage values must be finite and non-negative")
	}
	bucket := aggregate.Hour.UTC().Truncate(time.Hour)
	_, err := r.store.pool.Exec(ctx, `
		INSERT INTO usage_hourly (
			tenant_id, deployment_id, revision, bucket_start, input_tokens,
			output_tokens, requests, rejected_requests, gpu_seconds,
			shadow_gpu_seconds, estimated_cost_usd
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,0)
		ON CONFLICT (tenant_id, deployment_id, revision, bucket_start) DO UPDATE SET
			input_tokens=EXCLUDED.input_tokens,
			output_tokens=EXCLUDED.output_tokens,
			requests=EXCLUDED.requests,
			rejected_requests=EXCLUDED.rejected_requests,
			gpu_seconds=EXCLUDED.gpu_seconds,
			shadow_gpu_seconds=EXCLUDED.shadow_gpu_seconds`,
		aggregate.TenantID, aggregate.DeploymentID, aggregate.Revision, bucket,
		aggregate.InputTokens, aggregate.OutputTokens, aggregate.Requests,
		aggregate.RejectedRequests, aggregate.GPUSeconds, aggregate.ShadowGPUSeconds)
	return err
}
