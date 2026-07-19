-- name: UpsertDocument :one
INSERT INTO documents (
    source_uri,
    media_type,
    content_type,
    metadata
)
VALUES (
    sqlc.arg(source_uri),
    sqlc.arg(media_type),
    sqlc.arg(content_type),
    sqlc.arg(metadata)::jsonb
)
ON CONFLICT (source_uri) DO UPDATE
SET media_type = EXCLUDED.media_type,
    content_type = EXCLUDED.content_type,
    metadata = EXCLUDED.metadata,
    updated_at = CURRENT_TIMESTAMP
RETURNING id;

-- name: UpsertEmbedding :exec
INSERT INTO embeddings (
    document_id,
    model,
    segment_index,
    start_ms,
    end_ms,
    embedding,
    metadata
)
VALUES (
    sqlc.arg(document_id),
    sqlc.arg(model),
    sqlc.arg(segment_index),
    sqlc.narg(start_ms),
    sqlc.narg(end_ms),
    sqlc.arg(embedding)::vector,
    sqlc.arg(metadata)::jsonb
)
ON CONFLICT (document_id, model, segment_index) DO UPDATE
SET start_ms = EXCLUDED.start_ms,
    end_ms = EXCLUDED.end_ms,
    embedding = EXCLUDED.embedding,
    metadata = EXCLUDED.metadata,
    updated_at = CURRENT_TIMESTAMP;
