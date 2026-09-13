-- name: GetAPIKeyByPublicID :one
SELECT k.id, k.tenant_id, k.name, k.key_hash, k.key_salt,
       k.argon2_iterations, k.argon2_memory_kib, k.argon2_parallelism,
       k.scopes, k.created_at, k.expires_at, k.revoked_at, t.slug,
       t.namespace, t.suspended_at
FROM api_keys k JOIN tenants t ON t.id = k.tenant_id
WHERE k.id = $1;

-- name: GetTenant :one
SELECT id, slug, name, namespace, quota, created_at, updated_at, suspended_at
FROM tenants WHERE id = $1;
