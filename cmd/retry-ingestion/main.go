package main

import (
	"context"
	"flag"
	"fmt"
	"log"
	"os"
	"time"

	"semantic-search/internal/config"
	appdb "semantic-search/internal/database"
)

func main() {
	log.SetFlags(0)
	jobID := flag.Int64("id", 0, "failed ingestion job ID to requeue")
	flag.Parse()
	if *jobID <= 0 {
		fmt.Fprintln(os.Stderr, "usage: go run ./cmd/retry-ingestion -id <failed-job-id>")
		os.Exit(2)
	}
	if err := config.LoadDotEnv(); err != nil {
		log.Fatalf("configuration failed: %v", err)
	}

	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	db, err := appdb.Open(ctx)
	if err != nil {
		log.Fatalf("database connection failed: %v", err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		log.Fatalf("database pool failed: %v", err)
	}
	defer sqlDB.Close()

	if err := appdb.RequeueFailedIngestionJob(ctx, db, *jobID); err != nil {
		log.Fatal(err)
	}
	log.Printf("requeued ingestion job %d", *jobID)
}
