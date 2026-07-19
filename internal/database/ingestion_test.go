package database

import (
	"context"
	"strings"
	"testing"
)

func TestEnqueueIngestionJobValidatesNegativeObjectSize(t *testing.T) {
	store := &Store{}
	size := int64(-1)
	inserted, err := store.EnqueueIngestionJob(context.Background(), EnqueueIngestionJobInput{
		Bucket:    "semantic-search",
		ObjectKey: "incoming/photo.jpg",
		SizeBytes: &size,
	})
	if err == nil {
		t.Fatal("expected negative object size to fail validation")
	}
	if inserted {
		t.Fatal("negative object size was reported as inserted")
	}
	if !strings.Contains(err.Error(), "size must be non-negative") {
		t.Fatalf("error = %q, want size validation", err)
	}
}
