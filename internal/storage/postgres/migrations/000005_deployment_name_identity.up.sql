-- +goose Up

-- Earlier development schemas permitted a deleted deployment name to be
-- reused. Runtime metrics intentionally use the stable tenant-visible name,
-- so reuse would make historical usage ambiguous. Creating the full unique
-- index fails closed if an existing development database already contains
-- duplicate history; an operator must resolve that evidence explicitly.
CREATE UNIQUE INDEX IF NOT EXISTS deployments_tenant_name_idx
    ON deployments (tenant_id, name);

DROP INDEX IF EXISTS deployments_active_tenant_name_idx;
