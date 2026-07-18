package ingestion

import (
	"bytes"
	"context"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appdb "semantic-search/internal/database"
)

func TestHandleEventLogsDuplicateObject(t *testing.T) {
	var logs bytes.Buffer
	previousOutput := log.Writer()
	log.SetOutput(&logs)
	t.Cleanup(func() { log.SetOutput(previousOutput) })

	service := newEventService("test-token", func(context.Context, appdb.EnqueueIngestionJobInput) (bool, error) {
		return false, nil
	})
	body := `{"Records":[{"eventName":"s3:ObjectCreated:Put","s3":{"bucket":{"name":"semantic-search"},"object":{"key":"incoming%2Flaptop.jpg","eTag":"abc","size":42,"contentType":"image/jpeg"}}}]}`
	request := httptest.NewRequest(http.MethodPost, "/events/s3", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()

	service.HandleEvent(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("unexpected status: %d", response.Code)
	}
	if !strings.Contains(logs.String(), `S3 webhook object duplicate bucket="semantic-search" key="incoming/laptop.jpg"`) {
		t.Fatalf("missing duplicate log: %s", logs.String())
	}
}

func TestHandleEventEnqueuesCreatedObject(t *testing.T) {
	var inputs []appdb.EnqueueIngestionJobInput
	service := newEventService("test-token", func(_ context.Context, input appdb.EnqueueIngestionJobInput) (bool, error) {
		inputs = append(inputs, input)
		return true, nil
	})

	body := `{"Records":[{"eventName":"s3:ObjectCreated:Put","s3":{"bucket":{"name":"semantic-search"},"object":{"key":"incoming%2Fdrum+kit.jpg","versionId":"v1","eTag":"\"abc\"","size":42,"contentType":"image/jpeg"}}}]}`
	request := httptest.NewRequest(http.MethodPost, "/events/s3", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	service.HandleEvent(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if len(inputs) != 1 {
		t.Fatalf("enqueued %d jobs, want 1", len(inputs))
	}
	input := inputs[0]
	if input.Bucket != "semantic-search" || input.ObjectKey != "incoming/drum kit.jpg" || input.ObjectVersion != "v1" || input.ETag != "\"abc\"" {
		t.Fatalf("unexpected job: %#v", input)
	}
	if input.SizeBytes == nil || *input.SizeBytes != 42 || input.ContentType != "image/jpeg" {
		t.Fatalf("unexpected object metadata: %#v", input)
	}
}

func TestHandleEventRejectsInvalidToken(t *testing.T) {
	service := newEventService("test-token", func(context.Context, appdb.EnqueueIngestionJobInput) (bool, error) {
		t.Fatal("event was enqueued without authorization")
		return false, nil
	})
	response := httptest.NewRecorder()
	service.HandleEvent(response, httptest.NewRequest(http.MethodPost, "/events/s3", strings.NewReader(`{"Records":[]}`)))

	if response.Code != http.StatusUnauthorized {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusUnauthorized)
	}
}

func TestIsImage(t *testing.T) {
	for _, test := range []struct {
		contentType string
		key         string
		want        bool
	}{
		{"image/jpeg", "incoming/object", true},
		{"", "incoming/photo.png", true},
		{"video/mp4", "incoming/movie.mp4", false},
	} {
		if got := isImage(test.contentType, test.key); got != test.want {
			t.Errorf("isImage(%q, %q) = %t, want %t", test.contentType, test.key, got, test.want)
		}
	}
}
