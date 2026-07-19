package main

import (
	"context"
	"errors"
	"fmt"
	"log"
	"net/http"
	"os"
	"os/signal"
	"strings"
	"syscall"
	"time"

	"semantic-search/internal/config"
	appdb "semantic-search/internal/database"
	"semantic-search/internal/embedder"
	"semantic-search/internal/ingestion"
	"semantic-search/internal/storage"
)

const (
	defaultEmbedderURL = "http://127.0.0.1:8000"
	// RustFS shares the Linux host network and reaches this host-run service
	// through its semantic-search-ingester loopback alias.
	defaultAddress = "0.0.0.0:8081"
)

func main() {
	if err := config.LoadDotEnv(); err != nil {
		log.Fatalf("environment configuration failed: %v", err)
	}

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	db, err := appdb.Open(ctx)
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	defer db.Close()

	store, err := storage.NewFromEnvironment(ctx)
	if err != nil {
		log.Fatalf("S3 configuration failed: %v", err)
	}
	embedderURL := strings.TrimSpace(os.Getenv("EMBEDDER_URL"))
	if embedderURL == "" {
		embedderURL = defaultEmbedderURL
	}
	service := ingestion.New(db, store, embedder.NewClient(embedderURL), os.Getenv("INGESTION_WEBHOOK_TOKEN"))

	mux := http.NewServeMux()
	mux.HandleFunc("HEAD /{$}", func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusNoContent)
	})
	mux.HandleFunc("POST /events/s3", service.HandleEvent)
	mux.HandleFunc("GET /health", func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"status":"ok"}`))
	})

	address := strings.TrimSpace(os.Getenv("INGESTION_ADDR"))
	if address == "" {
		address = defaultAddress
	}
	server := &http.Server{Addr: address, Handler: mux, ReadHeaderTimeout: 5 * time.Second, IdleTimeout: 60 * time.Second}
	errCh := make(chan error, 2)
	go func() {
		log.Printf("ingestion event receiver listening on http://%s/events/s3", address)
		errCh <- server.ListenAndServe()
	}()
	go func() { errCh <- service.Run(ctx, time.Second) }()

	select {
	case <-ctx.Done():
		shutdownCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		if shutdownErr := server.Shutdown(shutdownCtx); shutdownErr != nil {
			log.Printf("ingestion server shutdown: %v", shutdownErr)
		}
	case err := <-errCh:
		if err != nil && !errors.Is(err, http.ErrServerClosed) {
			log.Fatal(fmt.Errorf("ingestion service: %w", err))
		}
	}
}
