package main

import (
	"context"
	"errors"
	"fmt"
	"mime"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"semantic-search/internal/config"
	appdb "semantic-search/internal/database"
	"semantic-search/internal/embedder"
	"semantic-search/internal/webapp"

	"gorm.io/gorm"
)

const defaultEmbedderURL = "http://127.0.0.1:8000"
const defaultHTTPAddress = "127.0.0.1:8080"

func previewLength(limit int, values []float64) int {
	if len(values) < limit {
		return len(values)
	}
	return limit
}

func main() {
	if len(os.Args) < 2 {
		printUsage()
		os.Exit(2)
	}
	if err := config.LoadDotEnv(); err != nil {
		fmt.Fprintf(os.Stderr, "Environment configuration failed: %v\n", err)
		os.Exit(1)
	}

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

	embedderURL := os.Getenv("EMBEDDER_URL")
	if embedderURL == "" {
		embedderURL = defaultEmbedderURL
	}
	client := embedder.NewClient(embedderURL)

	switch os.Args[1] {
	case "index":
		if len(os.Args) != 3 {
			printUsage()
			os.Exit(2)
		}
		err = indexImage(db, client, os.Args[2])
	case "search":
		if len(os.Args) < 3 {
			printUsage()
			os.Exit(2)
		}
		err = search(db, client, strings.Join(os.Args[2:], " "))
	case "serve":
		if len(os.Args) != 2 {
			printUsage()
			os.Exit(2)
		}
		err = serve(db, client)
	default:
		// Keep the original one-argument image indexing command working.
		if len(os.Args) != 2 {
			printUsage()
			os.Exit(2)
		}
		err = indexImage(db, client, os.Args[1])
	}

	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}
}

func serve(db *gorm.DB, client *embedder.Client) error {
	address := strings.TrimSpace(os.Getenv("HTTP_ADDR"))
	if address == "" {
		address = defaultHTTPAddress
	}

	server := &http.Server{
		Addr:              address,
		Handler:           webapp.New(db, client),
		ReadHeaderTimeout: 5 * time.Second,
		IdleTimeout:       60 * time.Second,
	}

	shutdownCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go func() {
		<-shutdownCtx.Done()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = server.Shutdown(ctx)
	}()

	fmt.Printf("Semantic search UI listening on http://%s\n", address)
	err := server.ListenAndServe()
	if errors.Is(err, http.ErrServerClosed) {
		return nil
	}
	return fmt.Errorf("serve semantic search UI: %w", err)
}

func indexImage(db *gorm.DB, client *embedder.Client, path string) error {
	imagePath, err := filepath.Abs(path)
	if err != nil {
		return fmt.Errorf("resolve image path: %w", err)
	}
	sourceURI := (&url.URL{Scheme: "file", Path: imagePath}).String()

	fmt.Printf("Generating embedding for %s...\n", imagePath)
	startTime := time.Now()
	embedding, err := client.GetFrameEmbedding(imagePath)
	if err != nil {
		return err
	}

	saveCtx, cancelSave := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelSave()
	if err := appdb.SaveDocumentEmbedding(saveCtx, db, appdb.SaveEmbeddingInput{
		SourceURI:   sourceURI,
		MediaType:   appdb.MediaTypeImage,
		ContentType: mime.TypeByExtension(filepath.Ext(imagePath)),
		Model:       embedding.Model,
		DocumentMetadata: appdb.Metadata{
			"local_path": imagePath,
		},
		EmbeddingMetadata: appdb.Metadata{
			"dimensions": len(embedding.Values),
		},
		Values: embedding.Values,
	}); err != nil {
		return fmt.Errorf("save embedding: %w", err)
	}

	preview := previewLength(5, embedding.Values)
	fmt.Printf("Generated vector in %v\n", time.Since(startTime))
	fmt.Printf("Vector dimensions: %d\n", len(embedding.Values))
	fmt.Printf("First %d dimensions: %v\n", preview, embedding.Values[:preview])
	fmt.Printf("Stored embedding for %s\n", sourceURI)
	return nil
}

func search(db *gorm.DB, client *embedder.Client, query string) error {
	fmt.Printf("Searching for %q...\n", query)
	embedding, err := client.GetTextEmbedding(query)
	if err != nil {
		return fmt.Errorf("embed text query: %w", err)
	}

	searchCtx, cancelSearch := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancelSearch()
	results, err := appdb.SearchDocuments(searchCtx, db, appdb.SearchInput{
		Values: embedding.Values,
		Model:  embedding.Model,
		Limit:  10,
	})
	if err != nil {
		return err
	}

	if len(results) == 0 {
		fmt.Println("No indexed documents found.")
		return nil
	}

	for index, result := range results {
		segment := ""
		if result.StartMS != nil || result.EndMS != nil {
			segment = fmt.Sprintf(
				" segment=%d start_ms=%s end_ms=%s",
				result.SegmentIndex,
				optionalMilliseconds(result.StartMS),
				optionalMilliseconds(result.EndMS),
			)
		}
		fmt.Printf("%d. similarity=%.4f%s %s\n", index+1, result.Similarity, segment, result.SourceURI)
	}
	return nil
}

func optionalMilliseconds(value *int64) string {
	if value == nil {
		return "-"
	}
	return strconv.FormatInt(*value, 10)
}

func printUsage() {
	fmt.Fprintln(os.Stderr, "usage:")
	fmt.Fprintln(os.Stderr, "  go run client.go index /path/to/image")
	fmt.Fprintln(os.Stderr, "  go run client.go search natural language query")
	fmt.Fprintln(os.Stderr, "  go run client.go serve")
}
