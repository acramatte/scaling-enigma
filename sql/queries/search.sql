-- name: SearchDocuments :many
SELECT
    d.id AS document_id,
    d.source_uri,
    d.media_type,
    d.content_type,
    d.metadata AS document_metadata,
    e.id AS embedding_id,
    e.segment_index,
    e.start_ms,
    e.end_ms,
    e.metadata AS embedding_metadata,
    (e.embedding <=> sqlc.arg(query_embedding)::vector)::double precision AS distance,
    (1 - (e.embedding <=> sqlc.arg(query_embedding)::vector))::double precision AS similarity
FROM embeddings AS e
JOIN documents AS d ON d.id = e.document_id
WHERE e.model = sqlc.arg(model)
ORDER BY e.embedding <=> sqlc.arg(query_embedding)::vector, e.id
LIMIT sqlc.arg(result_limit)::integer;

-- name: SearchDocumentsByMediaType :many
SELECT
    d.id AS document_id,
    d.source_uri,
    d.media_type,
    d.content_type,
    d.metadata AS document_metadata,
    e.id AS embedding_id,
    e.segment_index,
    e.start_ms,
    e.end_ms,
    e.metadata AS embedding_metadata,
    (e.embedding <=> sqlc.arg(query_embedding)::vector)::double precision AS distance,
    (1 - (e.embedding <=> sqlc.arg(query_embedding)::vector))::double precision AS similarity
FROM embeddings AS e
JOIN documents AS d ON d.id = e.document_id
WHERE e.model = sqlc.arg(model)
  AND d.media_type = sqlc.arg(media_type)
ORDER BY e.embedding <=> sqlc.arg(query_embedding)::vector, e.id
LIMIT sqlc.arg(result_limit)::integer;
