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

func TestEnqueueIngestionJobValidatesMediaClass(t *testing.T) {
	store := &Store{}
	inserted, err := store.EnqueueIngestionJob(context.Background(), EnqueueIngestionJobInput{
		Bucket:     "semantic-search",
		ObjectKey:  "incoming/photo.jpg",
		MediaClass: "audio",
	})
	if err == nil {
		t.Fatal("expected invalid media class to fail validation")
	}
	if inserted {
		t.Fatal("invalid media class was reported as inserted")
	}
	if !strings.Contains(err.Error(), "invalid ingestion job media class") {
		t.Fatalf("error = %q, want media class validation", err)
	}
}

func TestClaimIngestionJobByClassRejectsUnsupported(t *testing.T) {
	store := &Store{}
	for _, mediaClass := range []string{"", IngestionMediaClassUnsupported, "audio"} {
		job, err := store.ClaimIngestionJobByClass(context.Background(), mediaClass)
		if err == nil {
			t.Fatalf("ClaimIngestionJobByClass(%q) unexpectedly succeeded", mediaClass)
		}
		if job != nil {
			t.Fatalf("ClaimIngestionJobByClass(%q) returned job %#v", mediaClass, job)
		}
	}
}

func TestFailIngestionJobValidatesFailureKind(t *testing.T) {
	store := &Store{}
	for _, kind := range []string{"", "unknown", "permanent"} {
		err := store.FailIngestionJob(context.Background(), 1, "token", kind, "boom")
		if err == nil {
			t.Fatalf("FailIngestionJob(%q) unexpectedly succeeded", kind)
		}
		if !strings.Contains(err.Error(), "invalid ingestion failure kind") {
			t.Fatalf("FailIngestionJob(%q) error = %q, want failure kind validation", kind, err)
		}
	}
}

func TestClassifyMediaMetadata(t *testing.T) {
	tests := []struct {
		name        string
		contentType string
		objectKey   string
		want        string
	}{
		{"jpeg content type", "image/jpeg", "incoming/object", IngestionMediaClassImage},
		{"jpeg with charset parameter", "Image/PNG; charset=binary", "incoming/object", IngestionMediaClassImage},
		{"svg rejected as unsupported", "image/svg+xml", "incoming/diagram.svg", IngestionMediaClassUnsupported},
		{"mp4 content type", "video/mp4", "incoming/object", IngestionMediaClassVideo},
		{"octet-stream falls back to image ext", "application/octet-stream", "incoming/photo.WEBP", IngestionMediaClassImage},
		{"binary octet-stream falls back to video ext", "binary/octet-stream", "incoming/clip.mkv", IngestionMediaClassVideo},
		{"empty content type falls back to image ext with whitespace", "", " incoming/photo.jpg ", IngestionMediaClassImage},
		{"empty content type falls back to avi ext", "", "incoming/clip.avi", IngestionMediaClassVideo},
		{"empty content type falls back to avif ext", "", "incoming/frame.avif", IngestionMediaClassImage},
		{"explicit text overrides jpg extension", "text/plain; charset=utf-8", "incoming/readme.jpg", IngestionMediaClassUnsupported},
		{"pdf unsupported", "application/pdf", "incoming/doc.pdf", IngestionMediaClassUnsupported},
		{"unknown extension unsupported", "", "incoming/archive.tar.gz", IngestionMediaClassUnsupported},
		{"no extension and no content type", "", "incoming/blob", IngestionMediaClassUnsupported},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			if got := ClassifyMediaMetadata(tc.contentType, tc.objectKey); got != tc.want {
				t.Fatalf("ClassifyMediaMetadata(%q, %q) = %q, want %q", tc.contentType, tc.objectKey, got, tc.want)
			}
		})
	}
}
