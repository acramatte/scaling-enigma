package embedder

import (
	"bytes"
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestClientReturnsTypedStatusError(t *testing.T) {
	for _, code := range []int{http.StatusUnprocessableEntity, http.StatusRequestEntityTooLarge, http.StatusServiceUnavailable, http.StatusInternalServerError} {
		server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
			http.Error(w, "boom", code)
		}))
		client := NewClient(server.URL)

		_, err := client.GetImageEmbeddingContext(context.Background(), "sample.jpg", "image/jpeg", bytes.NewReader([]byte("x")))
		server.Close()

		var statusErr *StatusError
		if !errors.As(err, &statusErr) {
			t.Fatalf("status %d: expected *StatusError, got %T (%v)", code, err, err)
		}
		if statusErr.StatusCode != code {
			t.Fatalf("status = %d, want %d", statusErr.StatusCode, code)
		}
		if statusErr.Body != "boom" {
			t.Fatalf("body = %q, want %q", statusErr.Body, "boom")
		}
	}
}
