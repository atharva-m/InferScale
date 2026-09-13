-- +goose Down

CREATE UNIQUE INDEX IF NOT EXISTS deployments_active_tenant_name_idx
    ON deployments (tenant_id, name) WHERE deleted_at IS NULL;

DROP INDEX IF EXISTS deployments_tenant_name_idx;
