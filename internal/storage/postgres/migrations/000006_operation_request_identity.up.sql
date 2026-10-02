-- +goose Up
-- Historical requests cannot be reconstructed from mutable deployments.
-- Empty digests preserve those rows but fail closed on idempotency replay.
ALTER TABLE operations
    ADD COLUMN request_digest text NOT NULL DEFAULT '',
    ADD CONSTRAINT operations_request_digest_format
        CHECK (request_digest = '' OR request_digest ~ '^[a-f0-9]{64}$');
