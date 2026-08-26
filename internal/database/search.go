package database

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"strings"

	"semantic-search/internal/database/dbsql"
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
	SegmentIndex      int32
	StartMS           *int64
	EndMS             *int64
	EmbeddingMetadata Metadata
	Distance          float64
	Similarity        float64
}

// SearchDocuments returns the closest stored image or video-segment embeddings
// to a query embedding using pgvector cosine distance.
func (s *Store) SearchDocuments(ctx context.Context, input SearchInput) ([]SearchResult, error) {
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

	mediaType := strings.TrimSpace(input.MediaType)
	results := make([]SearchResult, 0)
	if mediaType == "" {
		rows, err := s.queries.SearchDocuments(ctx, dbsql.SearchDocumentsParams{
			QueryEmbedding: vector,
			Model:          model,
			ResultLimit:    int32(limit),
		})
		if err != nil {
			return nil, fmt.Errorf("search document embeddings: %w", err)
		}
		for _, row := range rows {
			result, err := newSearchResult(
				row.DocumentID, row.SourceUri, row.MediaType, row.ContentType,
				row.DocumentMetadata, row.EmbeddingID, row.SegmentIndex,
				row.StartMs, row.EndMs, row.EmbeddingMetadata,
				row.Distance, row.Similarity,
			)
			if err != nil {
				return nil, err
			}
			results = append(results, result)
		}
		return results, nil
	}

	rows, err := s.queries.SearchDocumentsByMediaType(ctx, dbsql.SearchDocumentsByMediaTypeParams{
		QueryEmbedding: vector,
		Model:          model,
		MediaType:      mediaType,
		ResultLimit:    int32(limit),
	})
	if err != nil {
		return nil, fmt.Errorf("search document embeddings: %w", err)
	}
	for _, row := range rows {
		result, err := newSearchResult(
			row.DocumentID, row.SourceUri, row.MediaType, row.ContentType,
			row.DocumentMetadata, row.EmbeddingID, row.SegmentIndex,
			row.StartMs, row.EndMs, row.EmbeddingMetadata,
			row.Distance, row.Similarity,
		)
		if err != nil {
			return nil, err
		}
		results = append(results, result)
	}
	return results, nil
}

func newSearchResult(
	documentID int64,
	sourceURI string,
	mediaType string,
	contentType string,
	documentMetadataJSON []byte,
	embeddingID int64,
	segmentIndex int32,
	startMS *int64,
	endMS *int64,
	embeddingMetadataJSON []byte,
	distance float64,
	similarity float64,
) (SearchResult, error) {
	var documentMetadata Metadata
	if err := json.Unmarshal(documentMetadataJSON, &documentMetadata); err != nil {
		return SearchResult{}, fmt.Errorf("decode document metadata: %w", err)
	}
	var embeddingMetadata Metadata
	if err := json.Unmarshal(embeddingMetadataJSON, &embeddingMetadata); err != nil {
		return SearchResult{}, fmt.Errorf("decode embedding metadata: %w", err)
	}
	return SearchResult{
		DocumentID:        documentID,
		SourceURI:         sourceURI,
		MediaType:         mediaType,
		ContentType:       contentType,
		DocumentMetadata:  documentMetadata,
		EmbeddingID:       embeddingID,
		SegmentIndex:      segmentIndex,
		StartMS:           startMS,
		EndMS:             endMS,
		EmbeddingMetadata: embeddingMetadata,
		Distance:          distance,
		Similarity:        similarity,
	}, nil
}
