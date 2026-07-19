-- +goose Up
CREATE EXTENSION IF NOT EXISTS vector;

CREATE TABLE documents (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    source_uri TEXT NOT NULL UNIQUE,
    media_type TEXT NOT NULL DEFAULT 'unknown',
    content_type TEXT NOT NULL DEFAULT '',
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now()
);

CREATE TABLE embeddings (
    id BIGINT GENERATED ALWAYS AS IDENTITY PRIMARY KEY,
    document_id BIGINT NOT NULL REFERENCES documents(id) ON DELETE CASCADE,
    model TEXT NOT NULL,
    segment_index INTEGER NOT NULL DEFAULT 0,
    start_ms BIGINT,
    end_ms BIGINT,
    embedding vector(768) NOT NULL,
    metadata JSONB NOT NULL DEFAULT '{}'::jsonb,
    created_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    updated_at TIMESTAMPTZ NOT NULL DEFAULT now(),
    CONSTRAINT embeddings_document_model_segment_key UNIQUE (document_id, model, segment_index),
    CONSTRAINT embeddings_segment_index_nonnegative CHECK (segment_index >= 0),
    CONSTRAINT embeddings_start_ms_nonnegative CHECK (start_ms IS NULL OR start_ms >= 0),
    CONSTRAINT embeddings_end_ms_nonnegative CHECK (end_ms IS NULL OR end_ms >= 0),
    CONSTRAINT embeddings_valid_time_range CHECK (start_ms IS NULL OR end_ms IS NULL OR end_ms >= start_ms)
);

-- Exact nearest-neighbor queries work without an index. Add an HNSW or IVFFlat
-- index once the table contains representative production data.

-- +goose Down
DROP TABLE IF EXISTS embeddings;
DROP TABLE IF EXISTS documents;
DROP EXTENSION IF EXISTS vector;
