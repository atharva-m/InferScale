-- +goose Up

ALTER TABLE benchmark_runs
    ADD COLUMN lease_owner text,
    ADD COLUMN lease_expires_at timestamptz,
    ADD COLUMN attempt_count integer NOT NULL DEFAULT 0 CHECK (attempt_count >= 0);

ALTER TABLE benchmark_runs
    ADD CONSTRAINT benchmark_runs_state_check
    CHECK (state IN ('queued', 'running', 'succeeded', 'failed'));

CREATE INDEX benchmark_runs_scheduler_idx
    ON benchmark_runs (state, lease_expires_at, created_at);

ALTER TABLE runtime_profiles
    ADD COLUMN model_uri text NOT NULL DEFAULT '';

-- The original skeleton accidentally made a compatibility key unique. Runtime
-- profiles are append-only measurements, so repeated runs of an identical
-- compatibility key must be retained as distinct evidence.
-- +goose StatementBegin
DO $$
DECLARE
    compatibility_constraint text;
BEGIN
    SELECT conname INTO compatibility_constraint
    FROM pg_constraint
    WHERE conrelid = 'runtime_profiles'::regclass
      AND contype = 'u'
      AND pg_get_constraintdef(oid) LIKE '%model_revision%'
      AND pg_get_constraintdef(oid) LIKE '%scenario_digest%'
    LIMIT 1;
    IF compatibility_constraint IS NOT NULL THEN
        EXECUTE format('ALTER TABLE runtime_profiles DROP CONSTRAINT %I', compatibility_constraint);
    END IF;
END $$;
-- +goose StatementEnd

CREATE INDEX runtime_profiles_compatibility_idx ON runtime_profiles (
    model_uri, model_revision, gpu_sku, gpu_count, precision, quantization,
    tensor_parallelism, max_context_bucket, scenario_digest, eligible
);
