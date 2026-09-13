-- +goose Down

ALTER TABLE benchmark_runs
    DROP CONSTRAINT IF EXISTS benchmark_runs_execution_snapshot_check;

ALTER TABLE benchmark_runs
    DROP COLUMN IF EXISTS execution_contract,
    DROP COLUMN IF EXISTS execution_digest,
    DROP COLUMN IF EXISTS execution_configuration;
