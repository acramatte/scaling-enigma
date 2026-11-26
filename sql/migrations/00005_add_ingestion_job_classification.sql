-- +goose Up
ALTER TABLE ingestion_jobs
    ADD COLUMN media_class TEXT NOT NULL DEFAULT 'unsupported',
    ADD COLUMN failure_kind TEXT NOT NULL DEFAULT '';

-- Backfill existing rows from persisted content_type or object_key extension.
UPDATE ingestion_jobs
SET media_class = CASE
    WHEN LOWER(TRIM(SPLIT_PART(content_type, ';', 1))) NOT IN ('', 'application/octet-stream', 'binary/octet-stream') THEN
        CASE
            WHEN LOWER(TRIM(SPLIT_PART(content_type, ';', 1))) LIKE 'image/%'
             AND LOWER(TRIM(SPLIT_PART(content_type, ';', 1))) <> 'image/svg+xml'
                THEN 'image'
            WHEN LOWER(TRIM(SPLIT_PART(content_type, ';', 1))) LIKE 'video/%'
                THEN 'video'
            ELSE 'unsupported'
        END
    WHEN LOWER(TRIM(object_key)) ~ '\.(jpg|jpeg|png|gif|webp|bmp|tif|tiff|avif|heic|heif)$'
        THEN 'image'
    WHEN LOWER(TRIM(object_key)) ~ '\.(mp4|mov|m4v|avi|mkv|webm|mpeg|mpg|ts|mts|m2ts|wmv|flv|3gp|ogv)$'
        THEN 'video'
    ELSE 'unsupported'
END;

-- Pre-existing active unsupported rows cannot be claimed by class-scoped image
-- or video workers. Mark them ignored cleanly before adding the constraint.
UPDATE ingestion_jobs
SET status = 'ignored',
    last_error = CASE WHEN last_error = '' THEN 'unsupported media type' ELSE last_error END,
    completed_at = COALESCE(completed_at, CURRENT_TIMESTAMP),
    lease_token = '',
    lease_expires_at = NULL,
    heartbeat_at = NULL,
    updated_at = CURRENT_TIMESTAMP
WHERE status IN ('pending', 'processing')
  AND media_class = 'unsupported';

ALTER TABLE ingestion_jobs
    ADD CONSTRAINT ingestion_jobs_media_class_valid CHECK (
        media_class IN ('image', 'video', 'unsupported')
    ),
    ADD CONSTRAINT ingestion_jobs_unsupported_not_active CHECK (
        media_class <> 'unsupported' OR status NOT IN ('pending', 'processing')
    ),
    ADD CONSTRAINT ingestion_jobs_failure_kind_valid CHECK (
        failure_kind IN ('', 'retryable', 'permanent_media', 'permanent_limit')
    ),
    ADD CONSTRAINT ingestion_jobs_failure_kind_state_valid CHECK (
        (status IN ('completed', 'ignored') AND failure_kind = '')
        OR status IN ('pending', 'processing', 'failed')
    );

CREATE INDEX ingestion_jobs_ready_class_idx ON ingestion_jobs (media_class, available_at, id)
WHERE status = 'pending';

CREATE INDEX ingestion_jobs_expired_lease_class_idx ON ingestion_jobs (media_class, lease_expires_at, id)
WHERE status = 'processing';

-- +goose Down
DROP INDEX IF EXISTS ingestion_jobs_expired_lease_class_idx;
DROP INDEX IF EXISTS ingestion_jobs_ready_class_idx;

ALTER TABLE ingestion_jobs
    DROP CONSTRAINT IF EXISTS ingestion_jobs_failure_kind_state_valid,
    DROP CONSTRAINT IF EXISTS ingestion_jobs_failure_kind_valid,
    DROP CONSTRAINT IF EXISTS ingestion_jobs_unsupported_not_active,
    DROP CONSTRAINT IF EXISTS ingestion_jobs_media_class_valid,
    DROP COLUMN IF EXISTS failure_kind,
    DROP COLUMN IF EXISTS media_class;
