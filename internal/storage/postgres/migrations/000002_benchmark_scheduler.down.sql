DROP INDEX IF EXISTS runtime_profiles_compatibility_idx;
ALTER TABLE runtime_profiles DROP COLUMN IF EXISTS model_uri;

DROP INDEX IF EXISTS benchmark_runs_scheduler_idx;
ALTER TABLE benchmark_runs DROP CONSTRAINT IF EXISTS benchmark_runs_state_check;
ALTER TABLE benchmark_runs
    DROP COLUMN IF EXISTS attempt_count,
    DROP COLUMN IF EXISTS lease_expires_at,
    DROP COLUMN IF EXISTS lease_owner;
