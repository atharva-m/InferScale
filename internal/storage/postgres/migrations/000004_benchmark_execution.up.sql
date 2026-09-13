-- +goose Up

-- The request configuration remains untouched for long-term idempotency.
-- These columns hold the server-authored execution snapshot that was frozen
-- after a scheduler claim and before its Kubernetes Job was created.
ALTER TABLE benchmark_runs
    ADD COLUMN execution_configuration jsonb,
    ADD COLUMN execution_digest text,
    ADD COLUMN execution_contract jsonb;

ALTER TABLE benchmark_runs
    ADD CONSTRAINT benchmark_runs_execution_snapshot_check CHECK (
        (execution_configuration IS NULL AND execution_digest IS NULL AND execution_contract IS NULL)
        OR
        (execution_configuration IS NOT NULL
         AND execution_digest ~ '^[0-9a-f]{64}$'
         AND execution_contract IS NOT NULL)
    );
