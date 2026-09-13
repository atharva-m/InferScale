-- name: GetTenantOperation :one
SELECT id, tenant_id, deployment_id, kind, status, request_id,
       idempotency_key, created_at, updated_at, completed_at, error
FROM operations WHERE tenant_id=$1 AND id=$2;

-- name: GetIdempotentOperation :one
SELECT id, tenant_id, deployment_id, kind, status, request_id,
       idempotency_key, created_at, updated_at, completed_at, error
FROM operations WHERE tenant_id=$1 AND idempotency_key=$2;
