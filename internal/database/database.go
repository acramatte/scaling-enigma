package database

import (
	"context"
	"database/sql"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"github.com/pgvector/pgvector-go"
	"gorm.io/driver/postgres"
	"gorm.io/gorm"
	"gorm.io/gorm/clause"
	"gorm.io/gorm/logger"
)

const defaultURL = "postgres://semantic_search:semantic_search@localhost:5432/semantic_search?sslmode=disable"

// Open creates a GORM connection pool and verifies that PostgreSQL is reachable.
// Set DATABASE_URL to override the local development connection string.
func Open(ctx context.Context) (*gorm.DB, error) {
	dsn := os.Getenv("DATABASE_URL")
	if dsn == "" {
		dsn = defaultURL
	}

	db, err := gorm.Open(postgres.Open(dsn), &gorm.Config{
		Logger: logger.Default.LogMode(logger.Warn),
	})
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}

	sqlDB, err := db.DB()
	if err != nil {
		return nil, fmt.Errorf("get postgres connection pool: %w", err)
	}
	sqlDB.SetMaxOpenConns(10)
	sqlDB.SetMaxIdleConns(5)
	sqlDB.SetConnMaxIdleTime(5 * time.Minute)
	sqlDB.SetConnMaxLifetime(time.Hour)

	if err := sqlDB.PingContext(ctx); err != nil {
		_ = sqlDB.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	return db, nil
}

type SaveEmbeddingInput struct {
	SourceURI         string
	MediaType         string
	ContentType       string
	DocumentMetadata  Metadata
	Model             string
	SegmentIndex      int
	StartMS           *int64
	EndMS             *int64
	EmbeddingMetadata Metadata
	Values            []float64
}

func SaveDocumentEmbedding(ctx context.Context, db *gorm.DB, input SaveEmbeddingInput) error {
	sourceURI := strings.TrimSpace(input.SourceURI)
	if sourceURI == "" {
		return fmt.Errorf("source URI is required")
	}
	vector, err := newVector(input.Values)
	if err != nil {
		return err
	}
	if input.SegmentIndex < 0 {
		return fmt.Errorf("segment index must be non-negative")
	}
	if input.StartMS != nil && *input.StartMS < 0 {
		return fmt.Errorf("start_ms must be non-negative")
	}
	if input.EndMS != nil && *input.EndMS < 0 {
		return fmt.Errorf("end_ms must be non-negative")
	}
	if input.StartMS != nil && input.EndMS != nil && *input.EndMS < *input.StartMS {
		return fmt.Errorf("end_ms must be greater than or equal to start_ms")
	}

	mediaType := strings.TrimSpace(input.MediaType)
	if mediaType == "" {
		mediaType = MediaTypeUnknown
	}

	model := strings.TrimSpace(input.Model)
	if model == "" {
		model = DefaultEmbeddingModel
	}

	documentMetadata := input.DocumentMetadata
	if documentMetadata == nil {
		documentMetadata = Metadata{}
	}

	embeddingMetadata := input.EmbeddingMetadata
	if embeddingMetadata == nil {
		embeddingMetadata = Metadata{}
	}

	return db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		document := Document{
			SourceURI:   sourceURI,
			MediaType:   mediaType,
			ContentType: strings.TrimSpace(input.ContentType),
			Metadata:    documentMetadata,
		}

		err := tx.Clauses(
			clause.OnConflict{
				Columns: []clause.Column{{Name: "source_uri"}},
				DoUpdates: clause.Assignments(map[string]any{
					"media_type":   gorm.Expr("EXCLUDED.media_type"),
					"content_type": gorm.Expr("EXCLUDED.content_type"),
					"metadata":     gorm.Expr("EXCLUDED.metadata"),
					"updated_at":   gorm.Expr("CURRENT_TIMESTAMP"),
				}),
			},
			clause.Returning{},
		).Create(&document).Error
		if err != nil {
			return fmt.Errorf("save document: %w", err)
		}

		embedding := Embedding{
			DocumentID:   document.ID,
			Model:        model,
			SegmentIndex: input.SegmentIndex,
			StartMS:      input.StartMS,
			EndMS:        input.EndMS,
			Vector:       vector,
			Metadata:     embeddingMetadata,
		}

		err = tx.Clauses(clause.OnConflict{
			Columns: []clause.Column{
				{Name: "document_id"},
				{Name: "model"},
				{Name: "segment_index"},
			},
			DoUpdates: clause.Assignments(map[string]any{
				"start_ms":   gorm.Expr("EXCLUDED.start_ms"),
				"end_ms":     gorm.Expr("EXCLUDED.end_ms"),
				"embedding":  gorm.Expr("EXCLUDED.embedding"),
				"metadata":   gorm.Expr("EXCLUDED.metadata"),
				"updated_at": gorm.Expr("CURRENT_TIMESTAMP"),
			}),
		}).Create(&embedding).Error
		if err != nil {
			return fmt.Errorf("save embedding: %w", err)
		}

		return nil
	})
}

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

func newVector(values []float64) (pgvector.Vector, error) {
	if len(values) != EmbeddingDimensions {
		return pgvector.Vector{}, fmt.Errorf(
			"embedding has %d dimensions, expected %d",
			len(values),
			EmbeddingDimensions,
		)
	}

	converted := make([]float32, len(values))
	for index, value := range values {
		if math.IsNaN(value) || math.IsInf(value, 0) {
			return pgvector.Vector{}, fmt.Errorf("embedding value at index %d is not finite", index)
		}
		converted[index] = float32(value)
	}

	return pgvector.NewVector(converted), nil
}
