-- +goose Up
-- Existing pre-lease workers have no fencing token. Return any job they left in
-- processing to the durable queue before making a non-empty lease mandatory.
ALTER TABLE ingestion_jobs
    ADD COLUMN lease_token TEXT NOT NULL DEFAULT '',
    ADD COLUMN lease_expires_at TIMESTAMPTZ,
    ADD COLUMN heartbeat_at TIMESTAMPTZ;

UPDATE ingestion_jobs
SET status = 'pending',
    available_at = CURRENT_TIMESTAMP,
    started_at = NULL,
    updated_at = CURRENT_TIMESTAMP
WHERE status = 'processing';

ALTER TABLE ingestion_jobs
    ADD CONSTRAINT ingestion_jobs_lease_state_valid CHECK (
        (status = 'processing' AND lease_token <> '' AND lease_expires_at IS NOT NULL)
        OR
        (status <> 'processing' AND lease_token = '' AND lease_expires_at IS NULL)
    );

CREATE INDEX ingestion_jobs_expired_lease_idx ON ingestion_jobs (lease_expires_at, id)
WHERE status = 'processing';

-- +goose Down
DROP INDEX IF EXISTS ingestion_jobs_expired_lease_idx;

ALTER TABLE ingestion_jobs
    DROP CONSTRAINT IF EXISTS ingestion_jobs_lease_state_valid,
    DROP COLUMN IF EXISTS heartbeat_at,
    DROP COLUMN IF EXISTS lease_expires_at,
    DROP COLUMN IF EXISTS lease_token;
