-- Forward-only in production. This downgrade is retained for disposable test
-- databases and requires callers to remove per-revision duplicates first.
ALTER TABLE usage_hourly DROP CONSTRAINT usage_hourly_pkey;
ALTER TABLE usage_hourly
    DROP COLUMN shadow_gpu_seconds,
    DROP COLUMN revision;
ALTER TABLE usage_hourly
    ADD PRIMARY KEY (tenant_id, deployment_id, bucket_start);
