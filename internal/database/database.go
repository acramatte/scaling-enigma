package database

import (
	"context"
	"fmt"
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
	if len(input.Values) != EmbeddingDimensions {
		return fmt.Errorf("embedding has %d dimensions, expected %d", len(input.Values), EmbeddingDimensions)
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

	vector := make([]float32, len(input.Values))
	for i, value := range input.Values {
		vector[i] = float32(value)
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
			Vector:       pgvector.NewVector(vector),
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
