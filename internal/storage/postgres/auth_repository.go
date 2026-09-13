package postgres

import (
	"context"
	"errors"
	"time"

	"github.com/inferscale/inferscale/internal/auth"
	"github.com/jackc/pgx/v5"
)

type AuthRepository struct{ store *Store }

func NewAuthRepository(store *Store) *AuthRepository { return &AuthRepository{store: store} }

func (r *AuthRepository) CreateAPIKey(ctx context.Context, key *auth.APIKey) error {
	_, err := r.store.pool.Exec(ctx, `
		INSERT INTO api_keys (
			id, tenant_id, name, key_hash, key_salt, argon2_iterations,
			argon2_memory_kib, argon2_parallelism, scopes, created_at, expires_at
		) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11)`,
		key.ID, key.TenantID, key.Name, key.Hash, key.Salt, key.Argon2.Iterations,
		key.Argon2.MemoryKiB, key.Argon2.Parallelism, key.Scopes, key.CreatedAt, key.ExpiresAt)
	return err
}

func (r *AuthRepository) LookupAPIKey(ctx context.Context, id string) (*auth.APIKey, *auth.Principal, error) {
	key := &auth.APIKey{}
	principal := &auth.Principal{}
	err := r.store.pool.QueryRow(ctx, `
		SELECT k.id, k.tenant_id, k.name, k.key_hash, k.key_salt,
		       k.argon2_iterations, k.argon2_memory_kib, k.argon2_parallelism,
		       k.scopes, k.created_at, k.expires_at, k.revoked_at, k.last_used_at,
		       t.slug, t.namespace, t.suspended_at
		FROM api_keys k JOIN tenants t ON t.id=k.tenant_id
		WHERE k.id=$1`, id).Scan(
		&key.ID, &key.TenantID, &key.Name, &key.Hash, &key.Salt,
		&key.Argon2.Iterations, &key.Argon2.MemoryKiB, &key.Argon2.Parallelism,
		&key.Scopes, &key.CreatedAt, &key.ExpiresAt, &key.RevokedAt, &key.LastUsedAt,
		&principal.TenantSlug, &principal.TenantNamespace, &principal.TenantSuspendedAt,
	)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil, auth.ErrKeyNotFound
	}
	if err != nil {
		return nil, nil, err
	}
	principal.APIKeyID = key.ID
	principal.TenantID = key.TenantID
	principal.Scopes = append([]string(nil), key.Scopes...)
	return key, principal, nil
}

func (r *AuthRepository) TouchAPIKey(ctx context.Context, id string, at time.Time) error {
	_, err := r.store.pool.Exec(ctx, `UPDATE api_keys SET last_used_at=$2 WHERE id=$1`, id, at)
	return err
}

func (r *AuthRepository) RevokeAPIKey(ctx context.Context, tenantID, id string, at time.Time) error {
	command, err := r.store.pool.Exec(ctx, `
		UPDATE api_keys SET revoked_at=$3
		WHERE tenant_id=$1 AND id=$2 AND revoked_at IS NULL`, tenantID, id, at)
	if err != nil {
		return err
	}
	if command.RowsAffected() == 0 {
		return auth.ErrKeyNotFound
	}
	return nil
}
