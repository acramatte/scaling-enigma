-- +goose Up
CREATE TABLE ingestion_jobs (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    bucket TEXT NOT NULL,
    object_key TEXT NOT NULL,
    object_version TEXT NOT NULL DEFAULT '',
    etag TEXT NOT NULL DEFAULT '',
    size_bytes BIGINT,
    content_type TEXT NOT NULL DEFAULT '',
    status TEXT NOT NULL DEFAULT 'pending',
    attempts INTEGER NOT NULL DEFAULT 0,
    last_error TEXT NOT NULL DEFAULT '',
    available_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    started_at TIMESTAMPTZ,
    completed_at TIMESTAMPTZ,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT ingestion_jobs_object_version_key UNIQUE (bucket, object_key, object_version, etag),
    CONSTRAINT ingestion_jobs_size_nonnegative CHECK (size_bytes IS NULL OR size_bytes >= 0),
    CONSTRAINT ingestion_jobs_attempts_nonnegative CHECK (attempts >= 0),
    CONSTRAINT ingestion_jobs_status_valid CHECK (status IN ('pending', 'processing', 'completed', 'ignored', 'failed'))
);

CREATE INDEX ingestion_jobs_ready_idx ON ingestion_jobs (available_at, id)
WHERE status = 'pending';

-- +goose Down
DROP TABLE IF EXISTS ingestion_jobs;
