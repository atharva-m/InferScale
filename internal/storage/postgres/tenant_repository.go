package postgres

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/inferscale/inferscale/internal/tenant"
	"github.com/jackc/pgx/v5"
)

type TenantRepository struct{ store *Store }

func NewTenantRepository(store *Store) *TenantRepository { return &TenantRepository{store: store} }

func (r *TenantRepository) Create(ctx context.Context, value *tenant.Tenant) error {
	quota, err := json.Marshal(value.Quota)
	if err != nil {
		return err
	}
	_, err = r.store.pool.Exec(ctx, `
		INSERT INTO tenants (id, slug, name, namespace, quota, created_at, updated_at, suspended_at)
		VALUES ($1,$2,$3,$4,$5,$6,$7,$8)`,
		value.ID, value.Slug, value.Name, value.Namespace, quota, value.CreatedAt, value.UpdatedAt, value.SuspendedAt)
	if uniqueViolation(err) {
		return tenant.ErrAlreadyExists
	}
	return err
}

func (r *TenantRepository) Get(ctx context.Context, id string) (*tenant.Tenant, error) {
	return r.get(ctx, `SELECT id, slug, name, namespace, quota, created_at, updated_at, suspended_at FROM tenants WHERE id=$1`, id)
}

func (r *TenantRepository) GetBySlug(ctx context.Context, slug string) (*tenant.Tenant, error) {
	return r.get(ctx, `SELECT id, slug, name, namespace, quota, created_at, updated_at, suspended_at FROM tenants WHERE slug=$1`, slug)
}

func (r *TenantRepository) get(ctx context.Context, query, value string) (*tenant.Tenant, error) {
	var result tenant.Tenant
	var quota []byte
	err := r.store.pool.QueryRow(ctx, query, value).Scan(
		&result.ID, &result.Slug, &result.Name, &result.Namespace, &quota, &result.CreatedAt, &result.UpdatedAt, &result.SuspendedAt)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, tenant.ErrNotFound
	}
	if err != nil {
		return nil, err
	}
	if err := json.Unmarshal(quota, &result.Quota); err != nil {
		return nil, err
	}
	return &result, nil
}

func (r *TenantRepository) List(ctx context.Context, limit, offset int) ([]tenant.Tenant, error) {
	rows, err := r.store.pool.Query(ctx, `
		SELECT id, slug, name, namespace, quota, created_at, updated_at, suspended_at
		FROM tenants ORDER BY created_at, id LIMIT $1 OFFSET $2`, limit, offset)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []tenant.Tenant
	for rows.Next() {
		var value tenant.Tenant
		var quota []byte
		if err := rows.Scan(&value.ID, &value.Slug, &value.Name, &value.Namespace, &quota, &value.CreatedAt, &value.UpdatedAt, &value.SuspendedAt); err != nil {
			return nil, err
		}
		if err := json.Unmarshal(quota, &value.Quota); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (r *TenantRepository) UpdateQuota(ctx context.Context, id string, quota tenant.Quota, now time.Time) error {
	payload, err := json.Marshal(quota)
	if err != nil {
		return err
	}
	command, err := r.store.pool.Exec(ctx, `UPDATE tenants SET quota=$2, updated_at=$3 WHERE id=$1`, id, payload, now)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return tenant.ErrNotFound
	}
	return nil
}

func (r *TenantRepository) SetSuspended(ctx context.Context, id string, suspendedAt *time.Time, updatedAt time.Time) error {
	command, err := r.store.pool.Exec(ctx, `UPDATE tenants SET suspended_at=$2, updated_at=$3 WHERE id=$1`, id, suspendedAt, updatedAt)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return tenant.ErrNotFound
	}
	return nil
}
