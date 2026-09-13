-- name: ListDeploymentBenchmarks :many
SELECT id, tenant_id, deployment_id, revision_id, scenario, scenario_digest,
       configuration, artifact_uri, provenance, idempotency_key, state, rental_price_usd_per_gpu_hour,
       created_at, started_at, completed_at, error
FROM benchmark_runs
WHERE tenant_id = $1 AND deployment_id = $2
ORDER BY created_at DESC, id DESC LIMIT $3 OFFSET $4;
