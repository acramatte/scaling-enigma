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
    lease_token = '',
    lease_expires_at = NULL,
    heartbeat_at = NULL,
    updated_at = CURRENT_TIMESTAMP
WHERE id = sqlc.arg(id)
  AND status = 'failed'
RETURNING id;

-- name: ClaimIngestionJob :one
WITH candidate AS (
    SELECT id
    FROM ingestion_jobs
    WHERE (status = 'pending' AND available_at <= CURRENT_TIMESTAMP)
       OR (status = 'processing' AND lease_expires_at <= CURRENT_TIMESTAMP)
    ORDER BY CASE WHEN status = 'pending' THEN available_at ELSE lease_expires_at END, id
    FOR UPDATE SKIP LOCKED
    LIMIT 1
)
UPDATE ingestion_jobs AS job
SET status = 'processing',
    attempts = job.attempts + 1,
    started_at = CURRENT_TIMESTAMP,
    lease_token = sqlc.arg(lease_token),
    lease_expires_at = CURRENT_TIMESTAMP + (sqlc.arg(lease_duration_milliseconds)::BIGINT * INTERVAL '1 millisecond'),
    heartbeat_at = CURRENT_TIMESTAMP,
    updated_at = CURRENT_TIMESTAMP
FROM candidate
WHERE job.id = candidate.id
  AND (
      (job.status = 'pending' AND job.available_at <= CURRENT_TIMESTAMP)
      OR (job.status = 'processing' AND job.lease_expires_at <= CURRENT_TIMESTAMP)
  )
RETURNING job.*;

-- name: CompleteIngestionJob :one
UPDATE ingestion_jobs
SET status = sqlc.arg(terminal_status),
    last_error = sqlc.arg(reason),
    completed_at = CURRENT_TIMESTAMP,
    lease_token = '',
    lease_expires_at = NULL,
    heartbeat_at = NULL,
    updated_at = CURRENT_TIMESTAMP
WHERE id = sqlc.arg(id)
  AND status = 'processing'
  AND lease_token = sqlc.arg(lease_token)
  AND lease_expires_at > CURRENT_TIMESTAMP
RETURNING id;

-- name: RetryIngestionJob :one
UPDATE ingestion_jobs
SET status = 'pending',
    available_at = sqlc.arg(available_at),
    last_error = sqlc.arg(reason),
    lease_token = '',
    lease_expires_at = NULL,
    heartbeat_at = NULL,
    updated_at = CURRENT_TIMESTAMP
WHERE id = sqlc.arg(id)
  AND status = 'processing'
  AND lease_token = sqlc.arg(lease_token)
  AND lease_expires_at > CURRENT_TIMESTAMP
RETURNING id;

-- name: HeartbeatIngestionJob :one
UPDATE ingestion_jobs
SET lease_expires_at = CURRENT_TIMESTAMP + (sqlc.arg(lease_duration_milliseconds)::BIGINT * INTERVAL '1 millisecond'),
    heartbeat_at = CURRENT_TIMESTAMP,
    updated_at = CURRENT_TIMESTAMP
WHERE id = sqlc.arg(id)
  AND status = 'processing'
  AND lease_token = sqlc.arg(lease_token)
  AND lease_expires_at > CURRENT_TIMESTAMP
RETURNING id;
