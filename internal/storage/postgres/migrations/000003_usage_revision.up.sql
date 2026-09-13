-- +goose Up
-- Usage is recomputed by immutable serving revision. Shadow GPU allocation is
-- retained separately because it is a platform rollout cost, not tenant token
-- usage.
ALTER TABLE usage_hourly DROP CONSTRAINT usage_hourly_pkey;
ALTER TABLE usage_hourly
    ADD COLUMN revision text NOT NULL DEFAULT 'unattributed',
    ADD COLUMN shadow_gpu_seconds double precision NOT NULL DEFAULT 0
        CHECK (shadow_gpu_seconds >= 0);
ALTER TABLE usage_hourly
    ADD PRIMARY KEY (tenant_id, deployment_id, revision, bucket_start);
