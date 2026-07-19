-- name: EnqueueIngestionJob :one
WITH inserted AS (
    INSERT INTO ingestion_jobs (
        bucket,
        object_key,
        object_version,
        etag,
        size_bytes,
        content_type
    )
    VALUES (
        sqlc.arg(bucket),
        sqlc.arg(object_key),
        sqlc.arg(object_version),
        sqlc.arg(etag),
        sqlc.narg(size_bytes),
        sqlc.arg(content_type)
    )
    ON CONFLICT (bucket, object_key, object_version, etag) DO NOTHING
    RETURNING id
)
SELECT EXISTS (SELECT 1 FROM inserted) AS inserted;

-- name: RequeueFailedIngestionJob :one
UPDATE ingestion_jobs
SET status = 'pending',
    attempts = 0,
    last_error = '',
    available_at = CURRENT_TIMESTAMP,
    started_at = NULL,
    completed_at = NULL,
    updated_at = CURRENT_TIMESTAMP
WHERE id = sqlc.arg(id)
  AND status = 'failed'
RETURNING id;

-- name: ClaimIngestionJob :one
WITH candidate AS (
    SELECT id
    FROM ingestion_jobs
    WHERE status = 'pending'
      AND available_at <= CURRENT_TIMESTAMP
    ORDER BY available_at, id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
UPDATE ingestion_jobs AS job
SET status = 'processing',
    attempts = job.attempts + 1,
    started_at = CURRENT_TIMESTAMP,
    updated_at = CURRENT_TIMESTAMP
FROM candidate
WHERE job.id = candidate.id
  AND job.status = 'pending'
RETURNING job.*;

-- name: CompleteIngestionJob :one
UPDATE ingestion_jobs
SET status = sqlc.arg(terminal_status),
    last_error = sqlc.arg(reason),
    completed_at = CURRENT_TIMESTAMP,
    updated_at = CURRENT_TIMESTAMP
WHERE id = sqlc.arg(id)
  AND status = 'processing'
RETURNING id;

-- name: RetryIngestionJob :one
UPDATE ingestion_jobs
SET status = 'pending',
    available_at = sqlc.arg(available_at),
    last_error = sqlc.arg(reason),
    updated_at = CURRENT_TIMESTAMP
WHERE id = sqlc.arg(id)
  AND status = 'processing'
RETURNING id;
