-- name: CreateDeployment :exec
INSERT INTO deployments (
  id, tenant_id, name, namespace, generation, spec, state,
  stable_revision_id, candidate_revision_id, created_at, updated_at
) VALUES ($1,$2,$3,$4,$5,$6,$7,$8,$9,$10,$11);

-- name: GetTenantDeployment :one
SELECT id, tenant_id, name, namespace, generation, spec, state,
       stable_revision_id, candidate_revision_id, observed_status,
       last_sync_error, created_at, updated_at, deleted_at
FROM deployments
WHERE tenant_id = $1 AND id = $2 AND deleted_at IS NULL;

-- name: ListTenantDeployments :many
SELECT id, tenant_id, name, namespace, generation, spec, state,
       stable_revision_id, candidate_revision_id, observed_status,
       last_sync_error, created_at, updated_at, deleted_at
FROM deployments
WHERE tenant_id = $1 AND deleted_at IS NULL
ORDER BY created_at DESC, id DESC LIMIT $2 OFFSET $3;
