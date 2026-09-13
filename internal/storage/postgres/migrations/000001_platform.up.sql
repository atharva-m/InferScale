-- +goose Up

CREATE TABLE IF NOT EXISTS schema_migrations (
    version text PRIMARY KEY,
    applied_at timestamptz NOT NULL DEFAULT now()
);

CREATE TABLE tenants (
    id text PRIMARY KEY,
    slug text NOT NULL UNIQUE,
    name text NOT NULL,
    namespace text NOT NULL UNIQUE,
    quota jsonb NOT NULL,
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    suspended_at timestamptz,
    CHECK (slug ~ '^[a-z0-9]([-a-z0-9]*[a-z0-9])?$')
);

CREATE TABLE api_keys (
    id text PRIMARY KEY,
    tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE CASCADE,
    name text NOT NULL,
    key_hash bytea NOT NULL UNIQUE,
    key_salt bytea NOT NULL,
    argon2_iterations integer NOT NULL,
    argon2_memory_kib integer NOT NULL,
    argon2_parallelism integer NOT NULL,
    scopes text[] NOT NULL,
    created_at timestamptz NOT NULL,
    expires_at timestamptz,
    revoked_at timestamptz,
    last_used_at timestamptz,
    UNIQUE (tenant_id, name)
);
CREATE INDEX api_keys_tenant_idx ON api_keys (tenant_id);

CREATE TABLE deployments (
    id text PRIMARY KEY,
    tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    name text NOT NULL,
    namespace text NOT NULL,
    generation bigint NOT NULL CHECK (generation > 0),
    spec jsonb NOT NULL,
    state text NOT NULL,
    stable_revision_id text,
    candidate_revision_id text,
    observed_status jsonb NOT NULL DEFAULT '{}'::jsonb,
    last_sync_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    deleted_at timestamptz
);
-- Deployment names are tenant-visible metric/model identities and are never
-- reused, including after soft deletion. This keeps historical time series
-- attributable without putting public UUIDs into every runtime label.
CREATE UNIQUE INDEX deployments_tenant_name_idx
    ON deployments (tenant_id, name);
CREATE INDEX deployments_tenant_updated_idx ON deployments (tenant_id, updated_at DESC);

CREATE TABLE deployment_revisions (
    id text PRIMARY KEY,
    deployment_id text NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    number bigint NOT NULL CHECK (number > 0),
    serving_digest text NOT NULL,
    spec jsonb NOT NULL,
    state text NOT NULL,
    requested_backend text NOT NULL,
    resolved_backend text,
    selection_status text NOT NULL,
    selected_profile_id text,
    runtime_image_digest text,
    created_at timestamptz NOT NULL,
    UNIQUE (deployment_id, number)
);
CREATE INDEX deployment_revisions_digest_idx
    ON deployment_revisions (deployment_id, serving_digest);

CREATE TABLE operations (
    id text PRIMARY KEY,
    tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    deployment_id text NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    kind text NOT NULL,
    status text NOT NULL,
    request_id text NOT NULL DEFAULT '',
    idempotency_key text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL,
    updated_at timestamptz NOT NULL,
    completed_at timestamptz,
    error text NOT NULL DEFAULT ''
);
CREATE INDEX operations_tenant_created_idx ON operations (tenant_id, created_at DESC);
CREATE UNIQUE INDEX operations_tenant_idempotency_idx
    ON operations (tenant_id, idempotency_key) WHERE idempotency_key <> '';

CREATE TABLE runtime_profiles (
    id text PRIMARY KEY,
    model_revision text NOT NULL,
    backend text NOT NULL,
    backend_version text NOT NULL,
    runtime_image_digest text NOT NULL,
    gpu_sku text NOT NULL,
    gpu_count integer NOT NULL,
    precision text NOT NULL,
    quantization text NOT NULL,
    tensor_parallelism integer NOT NULL,
    max_context_bucket integer NOT NULL,
    driver_cuda_fingerprint text NOT NULL,
    scenario_digest text NOT NULL,
    metrics jsonb NOT NULL,
    cost_per_successful_request double precision NOT NULL,
    eligible boolean NOT NULL DEFAULT false,
    approved_by text,
    approved_at timestamptz,
    measured_at timestamptz NOT NULL,
    source_benchmark_run_id text,
    CHECK (metrics ? 'cost_per_successful_request'),
    CHECK (NOT eligible OR (approved_by IS NOT NULL AND approved_at IS NOT NULL)),
    UNIQUE (model_revision, backend, backend_version, runtime_image_digest, gpu_sku, gpu_count, precision, quantization, tensor_parallelism, max_context_bucket, driver_cuda_fingerprint, scenario_digest)
);

CREATE TABLE benchmark_runs (
    id text PRIMARY KEY,
    tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    deployment_id text NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    revision_id text NOT NULL REFERENCES deployment_revisions(id) ON DELETE RESTRICT,
    scenario text NOT NULL,
    scenario_digest text NOT NULL,
    configuration jsonb NOT NULL,
    artifact_uri text NOT NULL DEFAULT '',
    provenance jsonb NOT NULL DEFAULT '{}'::jsonb,
    idempotency_key text NOT NULL,
    state text NOT NULL,
    rental_price_usd_per_gpu_hour double precision NOT NULL CHECK (rental_price_usd_per_gpu_hour >= 0),
    created_at timestamptz NOT NULL,
    started_at timestamptz,
    completed_at timestamptz,
    error text NOT NULL DEFAULT ''
);
CREATE INDEX benchmark_runs_deployment_created_idx
    ON benchmark_runs (deployment_id, created_at DESC);
CREATE UNIQUE INDEX benchmark_runs_tenant_idempotency_idx
    ON benchmark_runs (tenant_id, idempotency_key);

CREATE TABLE benchmark_results (
    benchmark_run_id text PRIMARY KEY REFERENCES benchmark_runs(id) ON DELETE CASCADE,
    provenance jsonb NOT NULL,
    metrics jsonb NOT NULL,
    created_at timestamptz NOT NULL DEFAULT now()
);

ALTER TABLE runtime_profiles
    ADD CONSTRAINT runtime_profiles_source_run_fk
    FOREIGN KEY (source_benchmark_run_id) REFERENCES benchmark_runs(id) ON DELETE RESTRICT;
ALTER TABLE deployment_revisions
    ADD CONSTRAINT deployment_revisions_profile_fk
    FOREIGN KEY (selected_profile_id) REFERENCES runtime_profiles(id) ON DELETE RESTRICT;

CREATE TABLE rollout_history (
    id bigserial PRIMARY KEY,
    deployment_id text NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    stable_revision_id text,
    candidate_revision_id text,
    stage text NOT NULL,
    reason text NOT NULL DEFAULT '',
    details jsonb NOT NULL DEFAULT '{}'::jsonb,
    created_at timestamptz NOT NULL
);

CREATE TABLE usage_hourly (
    tenant_id text NOT NULL REFERENCES tenants(id) ON DELETE RESTRICT,
    deployment_id text NOT NULL REFERENCES deployments(id) ON DELETE CASCADE,
    bucket_start timestamptz NOT NULL,
    input_tokens bigint NOT NULL DEFAULT 0,
    output_tokens bigint NOT NULL DEFAULT 0,
    requests bigint NOT NULL DEFAULT 0,
    rejected_requests bigint NOT NULL DEFAULT 0,
    gpu_seconds double precision NOT NULL DEFAULT 0,
    estimated_cost_usd double precision NOT NULL DEFAULT 0,
    PRIMARY KEY (tenant_id, deployment_id, bucket_start)
);

CREATE TABLE sync_outbox (
    id bigserial PRIMARY KEY,
    aggregate_type text NOT NULL,
    aggregate_id text NOT NULL,
    operation_id text NOT NULL REFERENCES operations(id) ON DELETE CASCADE,
    event_type text NOT NULL,
    payload jsonb NOT NULL DEFAULT '{}'::jsonb,
    attempts integer NOT NULL DEFAULT 0,
    next_attempt_at timestamptz NOT NULL DEFAULT now(),
    locked_until timestamptz,
    processed_at timestamptz,
    last_error text NOT NULL DEFAULT '',
    created_at timestamptz NOT NULL DEFAULT now()
);
CREATE INDEX sync_outbox_pending_idx
    ON sync_outbox (next_attempt_at, id) WHERE processed_at IS NULL;
