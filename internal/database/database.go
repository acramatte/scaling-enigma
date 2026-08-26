package database

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"math"
	"os"
	"strings"
	"time"

	"semantic-search/internal/database/dbsql"

	"github.com/jackc/pgx/v5/pgxpool"
	"github.com/pgvector/pgvector-go"
	pgxvector "github.com/pgvector/pgvector-go/pgx"
)

const defaultURL = "postgres://semantic_search:semantic_search@localhost:5432/semantic_search?sslmode=disable"

type Store struct {
	pool    *pgxpool.Pool
	queries *dbsql.Queries
}

// Open creates a pgx connection pool, registers pgvector's types on every
// connection, and verifies that PostgreSQL is reachable. Set DATABASE_URL to
// override the local development connection string.
func Open(ctx context.Context) (*Store, error) {
	dsn := strings.TrimSpace(os.Getenv("DATABASE_URL"))
	if dsn == "" {
		dsn = defaultURL
	}

	config, err := pgxpool.ParseConfig(dsn)
	if err != nil {
		return nil, fmt.Errorf("parse postgres configuration: %w", err)
	}
	config.MaxConns = 10
	config.MinIdleConns = 0
	config.MaxConnIdleTime = 5 * time.Minute
	config.MaxConnLifetime = time.Hour
	config.AfterConnect = pgxvector.RegisterTypes

	pool, err := pgxpool.NewWithConfig(ctx, config)
	if err != nil {
		return nil, fmt.Errorf("open postgres: %w", err)
	}
	if err := pool.Ping(ctx); err != nil {
		pool.Close()
		return nil, fmt.Errorf("ping postgres: %w", err)
	}

	return &Store{pool: pool, queries: dbsql.New(pool)}, nil
}

func (s *Store) Close() {
	if s != nil && s.pool != nil {
		s.pool.Close()
	}
}

type SaveEmbeddingInput struct {
	SourceURI         string
	MediaType         string
	ContentType       string
	DocumentMetadata  Metadata
	Model             string
	SegmentIndex      int32
	StartMS           *int64
	EndMS             *int64
	EmbeddingMetadata Metadata
	Values            []float64
}

func (s *Store) SaveDocumentEmbedding(ctx context.Context, input SaveEmbeddingInput) error {
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

	documentMetadata, err := marshalMetadata(input.DocumentMetadata)
	if err != nil {
		return fmt.Errorf("encode document metadata: %w", err)
	}
	embeddingMetadata, err := marshalMetadata(input.EmbeddingMetadata)
	if err != nil {
		return fmt.Errorf("encode embedding metadata: %w", err)
	}

	tx, err := s.pool.Begin(ctx)
	if err != nil {
		return fmt.Errorf("begin embedding transaction: %w", err)
	}
	defer func() { _ = tx.Rollback(ctx) }()
	queries := s.queries.WithTx(tx)

	documentID, err := queries.UpsertDocument(ctx, dbsql.UpsertDocumentParams{
		SourceUri:   sourceURI,
		MediaType:   mediaType,
		ContentType: strings.TrimSpace(input.ContentType),
		Metadata:    documentMetadata,
	})
	if err != nil {
		return fmt.Errorf("save document: %w", err)
	}
	if err := queries.UpsertEmbedding(ctx, dbsql.UpsertEmbeddingParams{
		DocumentID:   documentID,
		Model:        model,
		SegmentIndex: input.SegmentIndex,
		StartMs:      input.StartMS,
		EndMs:        input.EndMS,
		Embedding:    vector,
		Metadata:     embeddingMetadata,
	}); err != nil {
		return fmt.Errorf("save embedding: %w", err)
	}
	if err := tx.Commit(ctx); err != nil {
		return fmt.Errorf("commit embedding transaction: %w", err)
	}
	return nil
}

func marshalMetadata(metadata Metadata) ([]byte, error) {
	if metadata == nil {
		metadata = Metadata{}
	}
	return json.Marshal(metadata)
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
