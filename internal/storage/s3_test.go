package storage

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestCustomEndpointDoesNotRequestOptionalResponseChecksum(t *testing.T) {
	var checksumMode string
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		checksumMode = r.Header.Get("X-Amz-Checksum-Mode")
		if r.URL.Path != "/semantic-search/incoming/test.jpg" {
			t.Errorf("unexpected object path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", "image/jpeg")
		_, _ = w.Write([]byte("image data"))
	}))
	defer server.Close()

	t.Setenv("S3_ENDPOINT", server.URL)
	t.Setenv("S3_REGION", "us-east-1")
	t.Setenv("S3_ACCESS_KEY", "test-access-key")
	t.Setenv("S3_SECRET_KEY", "test-secret-key")

	store, err := NewFromEnvironment(context.Background())
	if err != nil {
		t.Fatalf("create store: %v", err)
	}
	object, err := store.Get(context.Background(), "semantic-search", "incoming/test.jpg", "")
	if err != nil {
		t.Fatalf("get object: %v", err)
	}
	defer object.Body.Close()
	if _, err := io.ReadAll(object.Body); err != nil {
		t.Fatalf("read object: %v", err)
	}
	if checksumMode != "" {
		t.Fatalf("custom endpoint unexpectedly received X-Amz-Checksum-Mode=%q", checksumMode)
	}
}
