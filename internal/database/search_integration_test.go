//go:build integration

package database

import (
	"context"
	"math"
	"testing"
	"time"

	appmigrations "semantic-search/migrations"

	"github.com/pressly/goose/v3"
	"github.com/testcontainers/testcontainers-go"
	postgrescontainer "github.com/testcontainers/testcontainers-go/modules/postgres"
	"gorm.io/gorm"
)

func TestSearchDocumentsIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()

	container, err := postgrescontainer.Run(
		ctx,
		"pgvector/pgvector:pg18-trixie",
		postgrescontainer.WithDatabase("semantic_search_test"),
		postgrescontainer.WithUsername("semantic_search"),
		postgrescontainer.WithPassword("semantic_search"),
		postgrescontainer.BasicWaitStrategies(),
	)
	if err != nil {
		t.Fatalf("start pgvector container: %v", err)
	}
	testcontainers.CleanupContainer(t, container)

	testDatabaseURL, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("get test database URL: %v", err)
	}
	t.Setenv("DATABASE_URL", testDatabaseURL)

	db, err := Open(ctx)
	if err != nil {
		t.Fatalf("open test database: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatalf("get test database pool: %v", err)
	}
	defer sqlDB.Close()
	provider, err := goose.NewProvider(goose.DialectPostgres, sqlDB, appmigrations.Files)
	if err != nil {
		t.Fatalf("create Goose provider: %v", err)
	}
	if _, err := provider.Up(ctx); err != nil {
		t.Fatalf("apply test migrations: %v", err)
	}

	tx := db.Begin()
	if tx.Error != nil {
		t.Fatalf("begin test transaction: %v", tx.Error)
	}
	defer tx.Rollback()

	startMS := int64(1_000)
	endMS := int64(2_000)
	saveIntegrationEmbedding(t, ctx, tx, SaveEmbeddingInput{
		SourceURI:   "test://search/exact-image",
		MediaType:   MediaTypeImage,
		ContentType: "image/jpeg",
		DocumentMetadata: Metadata{
			"label": "exact",
		},
		EmbeddingMetadata: Metadata{
			"kind": "image",
		},
		Values: integrationVector(1, 0),
	})
	saveIntegrationEmbedding(t, ctx, tx, SaveEmbeddingInput{
		SourceURI:    "test://search/video-segment",
		MediaType:    MediaTypeVideo,
		ContentType:  "video/mp4",
		SegmentIndex: 3,
		StartMS:      &startMS,
		EndMS:        &endMS,
		DocumentMetadata: Metadata{
			"label": "video",
		},
		EmbeddingMetadata: Metadata{
			"kind": "frame",
		},
		Values: integrationVector(0.8, 0.6),
	})
	saveIntegrationEmbedding(t, ctx, tx, SaveEmbeddingInput{
		SourceURI: "test://search/other-model",
		MediaType: MediaTypeImage,
		Model:     "different-checkpoint",
		Values:    integrationVector(1, 0),
	})

	results, err := SearchDocuments(ctx, tx, SearchInput{
		Values: integrationVector(1, 0),
	})
	if err != nil {
		t.Fatalf("search documents: %v", err)
	}
	if len(results) != 2 {
		t.Fatalf("expected 2 results from the default model, got %d", len(results))
	}

	exact := results[0]
	if exact.SourceURI != "test://search/exact-image" {
		t.Errorf("expected exact image first, got %q", exact.SourceURI)
	}
	if math.Abs(exact.Similarity-1) > 1e-6 {
		t.Errorf("expected exact similarity 1, got %f", exact.Similarity)
	}
	if exact.DocumentMetadata["label"] != "exact" {
		t.Errorf("document metadata was not scanned: %#v", exact.DocumentMetadata)
	}
	if exact.EmbeddingMetadata["kind"] != "image" {
		t.Errorf("embedding metadata was not scanned: %#v", exact.EmbeddingMetadata)
	}

	segment := results[1]
	if segment.SourceURI != "test://search/video-segment" {
		t.Errorf("expected video segment second, got %q", segment.SourceURI)
	}
	if math.Abs(segment.Similarity-0.8) > 1e-5 {
		t.Errorf("expected video similarity 0.8, got %f", segment.Similarity)
	}
	if segment.SegmentIndex != 3 {
		t.Errorf("expected segment index 3, got %d", segment.SegmentIndex)
	}
	if segment.StartMS == nil {
		t.Error("expected start_ms to be present")
	} else if *segment.StartMS != startMS {
		t.Errorf("expected start_ms %d, got %d", startMS, *segment.StartMS)
	}
	if segment.EndMS == nil {
		t.Error("expected end_ms to be present")
	} else if *segment.EndMS != endMS {
		t.Errorf("expected end_ms %d, got %d", endMS, *segment.EndMS)
	}

	videoResults, err := SearchDocuments(ctx, tx, SearchInput{
		Values:    integrationVector(1, 0),
		MediaType: MediaTypeVideo,
	})
	if err != nil {
		t.Fatalf("search videos: %v", err)
	}
	if len(videoResults) != 1 || videoResults[0].SourceURI != "test://search/video-segment" {
		t.Errorf("media filter returned unexpected results: %#v", videoResults)
	}

	otherModelResults, err := SearchDocuments(ctx, tx, SearchInput{
		Values: integrationVector(1, 0),
		Model:  "different-checkpoint",
	})
	if err != nil {
		t.Fatalf("search other model: %v", err)
	}
	if len(otherModelResults) != 1 || otherModelResults[0].SourceURI != "test://search/other-model" {
		t.Errorf("model filter returned unexpected results: %#v", otherModelResults)
	}

	limitedResults, err := SearchDocuments(ctx, tx, SearchInput{
		Values: integrationVector(1, 0),
		Limit:  1,
	})
	if err != nil {
		t.Fatalf("search with limit: %v", err)
	}
	if len(limitedResults) != 1 || limitedResults[0].SourceURI != "test://search/exact-image" {
		t.Errorf("limit returned unexpected results: %#v", limitedResults)
	}
}

func saveIntegrationEmbedding(t *testing.T, ctx context.Context, db *gorm.DB, input SaveEmbeddingInput) {
	t.Helper()
	if err := SaveDocumentEmbedding(ctx, db, input); err != nil {
		t.Fatalf("save integration embedding %q: %v", input.SourceURI, err)
	}
}

func integrationVector(first, second float64) []float64 {
	values := make([]float64, EmbeddingDimensions)
	values[0] = first
	values[1] = second
	return values
}
