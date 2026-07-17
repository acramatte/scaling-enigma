package database

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"gorm.io/gorm"
)

type SearchInput struct {
	Values    []float64
	Model     string
	MediaType string
	Limit     int
}

type SearchResult struct {
	DocumentID        int64
	SourceURI         string
	MediaType         string
	ContentType       string
	DocumentMetadata  Metadata
	EmbeddingID       int64
	SegmentIndex      int
	StartMS           *int64
	EndMS             *int64
	EmbeddingMetadata Metadata
	Distance          float64
	Similarity        float64
}

// SearchDocuments returns the closest stored image or video-segment embeddings
// to a query embedding using pgvector cosine distance.
func SearchDocuments(ctx context.Context, db *gorm.DB, input SearchInput) ([]SearchResult, error) {
	vector, err := newVector(input.Values)
	if err != nil {
		return nil, err
	}

	model := strings.TrimSpace(input.Model)
	if model == "" {
		model = DefaultEmbeddingModel
	}

	limit := input.Limit
	if limit <= 0 {
		limit = 10
	}
	if limit > 100 {
		return nil, fmt.Errorf("search limit must not exceed 100")
	}

	query := `
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
			e.embedding <=> @embedding AS distance,
			1 - (e.embedding <=> @embedding) AS similarity
		FROM embeddings AS e
		JOIN documents AS d ON d.id = e.document_id
		WHERE e.model = @model`

	arguments := []any{
		sql.Named("embedding", vector),
		sql.Named("model", model),
		sql.Named("limit", limit),
	}
	if mediaType := strings.TrimSpace(input.MediaType); mediaType != "" {
		query += " AND d.media_type = @media_type"
		arguments = append(arguments, sql.Named("media_type", mediaType))
	}
	query += " ORDER BY e.embedding <=> @embedding LIMIT @limit"

	var results []SearchResult
	if err := db.WithContext(ctx).Raw(query, arguments...).Scan(&results).Error; err != nil {
		return nil, fmt.Errorf("search document embeddings: %w", err)
	}

	return results, nil
}
