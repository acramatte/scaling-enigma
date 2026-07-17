package main

import (
	"context"
	"fmt"
	"mime"
	"net/url"
	"os"
	"path/filepath"
	"time"

	appdb "semantic-search/internal/database"
	"semantic-search/internal/embedder"
)

func previewLength(limit int, values []float64) int {
	if len(values) < limit {
		return len(values)
	}
	return limit
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: go run client.go /path/to/image\n")
		os.Exit(2)
	}

	imagePath, err := filepath.Abs(os.Args[1])
	if err != nil {
		fmt.Fprintf(os.Stderr, "Invalid image path: %v\n", err)
		os.Exit(1)
	}
	sourceURI := (&url.URL{Scheme: "file", Path: imagePath}).String()

	connectCtx, cancelConnect := context.WithTimeout(context.Background(), 5*time.Second)
	db, err := appdb.Open(connectCtx)
	cancelConnect()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Database connection failed: %v\n", err)
		os.Exit(1)
	}
	sqlDB, err := db.DB()
	if err != nil {
		fmt.Fprintf(os.Stderr, "Database pool failed: %v\n", err)
		os.Exit(1)
	}
	defer sqlDB.Close()

	client := embedder.NewClient("http://127.0.0.1:8000")

	fmt.Printf("Generating embedding for %s...\n", imagePath)
	startTime := time.Now()

	vector, err := client.GetFrameEmbedding(imagePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	duration := time.Since(startTime)
	preview := previewLength(5, vector)

	saveCtx, cancelSave := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelSave()
	if err := appdb.SaveDocumentEmbedding(saveCtx, db, appdb.SaveEmbeddingInput{
		SourceURI:   sourceURI,
		MediaType:   appdb.MediaTypeImage,
		ContentType: mime.TypeByExtension(filepath.Ext(imagePath)),
		Model:       appdb.DefaultEmbeddingModel,
		DocumentMetadata: appdb.Metadata{
			"local_path": imagePath,
		},
		EmbeddingMetadata: appdb.Metadata{
			"dimensions": len(vector),
		},
		Values: vector,
	}); err != nil {
		fmt.Fprintf(os.Stderr, "Database save failed: %v\n", err)
		os.Exit(1)
	}

	fmt.Printf("Successfully generated vector in %v!\n", duration)
	fmt.Printf("Vector Dimensions: %d\n", len(vector))
	fmt.Printf("First %d Dimensions: %v\n", preview, vector[:preview])
	fmt.Printf("Stored embedding for %s\n", sourceURI)
}
