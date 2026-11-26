package ingestion

import (
	"bytes"
	"context"
	"errors"
	"io"
	"log"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	appdb "semantic-search/internal/database"
	"semantic-search/internal/embedder"
	"semantic-search/internal/storage"

	"github.com/aws/smithy-go"
)

type stubFetcher struct {
	get func(ctx context.Context, bucket, key, version string) (storage.Object, error)
}

func (f stubFetcher) Get(ctx context.Context, bucket, key, version string) (storage.Object, error) {
	return f.get(ctx, bucket, key, version)
}

type stubEmbedder struct {
	embed func(ctx context.Context, filename, contentType string, image io.Reader) (embedder.Result, error)
}

func (e stubEmbedder) GetImageEmbeddingContext(ctx context.Context, filename, contentType string, image io.Reader) (embedder.Result, error) {
	return e.embed(ctx, filename, contentType, image)
}

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
	if !strings.Contains(logs.String(), `S3 webhook object duplicate bucket="semantic-search" key="incoming/laptop.jpg" media_class="image"`) {
		t.Fatalf("missing duplicate log: %s", logs.String())
	}
}

func TestHandleEventEnqueuesClassifiedObjects(t *testing.T) {
	var inputs []appdb.EnqueueIngestionJobInput
	service := newEventService("test-token", func(_ context.Context, input appdb.EnqueueIngestionJobInput) (bool, error) {
		inputs = append(inputs, input)
		return true, nil
	})

	body := `{"Records":[
		{"eventName":"s3:ObjectCreated:Put","s3":{"bucket":{"name":"semantic-search"},"object":{"key":"incoming%2Fdrum+kit.jpg","versionId":"v1","eTag":"\"abc\"","size":42,"contentType":"image/jpeg"}}},
		{"eventName":"s3:ObjectCreated:Put","s3":{"bucket":{"name":"semantic-search"},"object":{"key":"incoming%2Fclip.mp4","versionId":"v2","eTag":"\"def\"","size":1024,"contentType":"video/mp4"}}},
		{"eventName":"s3:ObjectCreated:Put","s3":{"bucket":{"name":"semantic-search"},"object":{"key":"incoming%2Fnotes.txt","versionId":"v3","eTag":"\"ghi\"","size":12,"contentType":"text/plain"}}}
	]}`
	request := httptest.NewRequest(http.MethodPost, "/events/s3", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer test-token")
	response := httptest.NewRecorder()
	service.HandleEvent(response, request)

	if response.Code != http.StatusNoContent {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNoContent)
	}
	if len(inputs) != 3 {
		t.Fatalf("enqueued %d jobs, want 3", len(inputs))
	}
	if inputs[0].ObjectKey != "incoming/drum kit.jpg" || inputs[0].MediaClass != appdb.IngestionMediaClassImage {
		t.Fatalf("unexpected image job: %#v", inputs[0])
	}
	if inputs[1].ObjectKey != "incoming/clip.mp4" || inputs[1].MediaClass != appdb.IngestionMediaClassVideo {
		t.Fatalf("unexpected video job: %#v", inputs[1])
	}
	if inputs[2].ObjectKey != "incoming/notes.txt" || inputs[2].MediaClass != appdb.IngestionMediaClassUnsupported {
		t.Fatalf("unexpected unsupported job: %#v", inputs[2])
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

func TestVerifyImageMedia(t *testing.T) {
	validJPEG := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10, 'J', 'F', 'I', 'F'}
	validPNG := []byte{0x89, 'P', 'N', 'G', 0x0D, 0x0A, 0x1A, 0x0A, 0x00}
	validWebP := []byte("RIFF\x24\x00\x00\x00WEBP")
	validAVIF := []byte{0x00, 0x00, 0x00, 0x1C, 'f', 't', 'y', 'p', 'a', 'v', 'i', 'f'}
	validMP4 := []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}
	plainText := []byte("hello world, not an image")

	for _, tc := range []struct {
		name      string
		s3Type    string
		payload   []byte
		wantMIME  string
		wantError bool
	}{
		{"valid jpeg with image/jpeg", "image/jpeg", validJPEG, "image/jpeg", false},
		{"valid png with generic octet-stream", "application/octet-stream", validPNG, "image/png", false},
		{"valid webp with empty s3 header", "", validWebP, "image/webp", false},
		{"valid avif", "image/avif", validAVIF, "image/avif", false},
		{"empty payload rejected", "image/jpeg", nil, "", true},
		{"text disguised as jpg rejected", "image/jpeg", plainText, "", true},
		{"mp4 disguised as jpg rejected", "image/jpeg", validMP4, "", true},
		{"s3 video header overrides jpg bytes", "video/mp4", validJPEG, "", true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := VerifyImageMedia(tc.s3Type, "incoming/sample.jpg", tc.payload)
			if tc.wantError {
				if err == nil {
					t.Fatalf("expected error, got mime %q", got)
				}
				kind, retryable := ClassifyError(err)
				if retryable || kind != appdb.IngestionFailurePermanentMedia {
					t.Fatalf("ClassifyError = (%q, retryable=%t), want (%q, false)", kind, retryable, appdb.IngestionFailurePermanentMedia)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tc.wantMIME {
				t.Fatalf("verified MIME = %q, want %q", got, tc.wantMIME)
			}
		})
	}
}

func TestVerifyVideoMedia(t *testing.T) {
	validMP4 := []byte{0x00, 0x00, 0x00, 0x18, 'f', 't', 'y', 'p', 'i', 's', 'o', 'm'}
	validAVI := []byte("RIFF\x24\x00\x00\x00AVI ")
	validWebM := []byte{0x1A, 0x45, 0xDF, 0xA3, 0x9F}
	validJPEG := []byte{0xFF, 0xD8, 0xFF, 0xE0}

	if got, err := VerifyVideoMedia("video/mp4", "incoming/clip.mp4", validMP4); err != nil || got != "video/mp4" {
		t.Fatalf("mp4 verify = (%q, %v), want (video/mp4, nil)", got, err)
	}
	if got, err := VerifyVideoMedia("application/octet-stream", "incoming/clip.avi", validAVI); err != nil || got != "video/x-msvideo" {
		t.Fatalf("avi verify = (%q, %v), want (video/x-msvideo, nil)", got, err)
	}
	if got, err := VerifyVideoMedia("", "incoming/clip.mkv", validWebM); err != nil || got != "video/webm" {
		t.Fatalf("webm verify = (%q, %v), want (video/webm, nil)", got, err)
	}
	if _, err := VerifyVideoMedia("video/mp4", "incoming/clip.mp4", validJPEG); err == nil {
		t.Fatal("expected image bytes disguised as video to fail verification")
	}
}

func TestClassifyErrorPermanentVsRetryable(t *testing.T) {
	tests := []struct {
		name          string
		err           error
		wantKind      string
		wantRetryable bool
	}{
		{"embedder 422 permanent media", &embedder.StatusError{StatusCode: 422, Body: "bad image"}, appdb.IngestionFailurePermanentMedia, false},
		{"embedder 413 permanent limit", &embedder.StatusError{StatusCode: 413, Body: "too large"}, appdb.IngestionFailurePermanentLimit, false},
		{"embedder 503 retryable", &embedder.StatusError{StatusCode: 503, Body: "saturated"}, appdb.IngestionFailureRetryable, true},
		{"embedder 500 retryable", &embedder.StatusError{StatusCode: 500, Body: "oops"}, appdb.IngestionFailureRetryable, true},
		{"s3 NoSuchKey permanent media", &smithy.GenericAPIError{Code: "NoSuchKey", Message: "missing"}, appdb.IngestionFailurePermanentMedia, false},
		{"s3 SlowDown retryable", &smithy.GenericAPIError{Code: "SlowDown", Message: "busy"}, appdb.IngestionFailureRetryable, true},
		{"network error retryable", errors.New("connection refused"), appdb.IngestionFailureRetryable, true},
		{"explicit permanent limit", PermanentLimitError(errors.New("too large")), appdb.IngestionFailurePermanentLimit, false},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			gotKind, gotRetryable := ClassifyError(tc.err)
			if gotKind != tc.wantKind || gotRetryable != tc.wantRetryable {
				t.Fatalf("ClassifyError(%v) = (%q, %t), want (%q, %t)", tc.err, gotKind, gotRetryable, tc.wantKind, tc.wantRetryable)
			}
		})
	}
}

func TestProcessClaimedSeparatesPermanentAndRetryableFailures(t *testing.T) {
	validJPEG := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}

	t.Run("oversized image fails permanently on attempt 1 without retry", func(t *testing.T) {
		var failedKind, failedReason string
		retried := false
		size := int64(2048)
		s := &Service{
			maxImageBytes: 1024,
			heartbeatJob:  func(context.Context, int64, string) error { return nil },
			failJob: func(_ context.Context, _ int64, _, kind, reason string) error {
				failedKind, failedReason = kind, reason
				return nil
			},
			retryJob: func(context.Context, int64, string, time.Time, string) error {
				retried = true
				return nil
			},
		}
		err := s.processClaimedWithLease(context.Background(), &appdb.IngestionJob{
			ID: 1, Bucket: "b", ObjectKey: "incoming/huge.jpg", MediaClass: appdb.IngestionMediaClassImage,
			SizeBytes: &size, Attempts: 1, LeaseToken: "tok",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if retried {
			t.Fatal("oversized image was retried instead of failing permanently")
		}
		if failedKind != appdb.IngestionFailurePermanentLimit || !strings.Contains(failedReason, "exceeds limit") {
			t.Fatalf("failed = (%q, %q), want permanent_limit", failedKind, failedReason)
		}
	})

	t.Run("corrupt image disguised as jpeg fails permanently on attempt 1", func(t *testing.T) {
		var failedKind string
		retried := false
		s := &Service{
			maxImageBytes: 1024,
			store: stubFetcher{get: func(context.Context, string, string, string) (storage.Object, error) {
				return storage.Object{
					Body:        io.NopCloser(strings.NewReader("<html>not an image</html>")),
					ContentType: "image/jpeg",
				}, nil
			}},
			heartbeatJob: func(context.Context, int64, string) error { return nil },
			failJob: func(_ context.Context, _ int64, _, kind, _ string) error {
				failedKind = kind
				return nil
			},
			retryJob: func(context.Context, int64, string, time.Time, string) error {
				retried = true
				return nil
			},
		}
		err := s.processClaimedWithLease(context.Background(), &appdb.IngestionJob{
			ID: 2, Bucket: "b", ObjectKey: "incoming/fake.jpg", MediaClass: appdb.IngestionMediaClassImage,
			Attempts: 1, LeaseToken: "tok",
		})
		if err != nil {
			t.Fatalf("unexpected error: %v", err)
		}
		if retried || failedKind != appdb.IngestionFailurePermanentMedia {
			t.Fatalf("retried=%t failedKind=%q, want retried=false permanent_media", retried, failedKind)
		}
	})

	t.Run("embedder 503 retries on attempt 1 and fails as retryable on attempt 3", func(t *testing.T) {
		var retriedCount int
		var failedKind string
		s := &Service{
			maxImageBytes: 1024,
			store: stubFetcher{get: func(context.Context, string, string, string) (storage.Object, error) {
				return storage.Object{Body: io.NopCloser(bytes.NewReader(validJPEG)), ContentType: "image/jpeg"}, nil
			}},
			embedder: stubEmbedder{embed: func(context.Context, string, string, io.Reader) (embedder.Result, error) {
				return embedder.Result{}, &embedder.StatusError{StatusCode: http.StatusServiceUnavailable, Body: "busy"}
			}},
			heartbeatJob: func(context.Context, int64, string) error { return nil },
			failJob: func(_ context.Context, _ int64, _, kind, _ string) error {
				failedKind = kind
				return nil
			},
			retryJob: func(context.Context, int64, string, time.Time, string) error {
				retriedCount++
				return nil
			},
		}

		// Attempt 1 -> retry
		if err := s.processClaimedWithLease(context.Background(), &appdb.IngestionJob{
			ID: 3, Bucket: "b", ObjectKey: "incoming/ok.jpg", MediaClass: appdb.IngestionMediaClassImage,
			Attempts: 1, LeaseToken: "tok1",
		}); err != nil {
			t.Fatalf("attempt 1: %v", err)
		}
		if retriedCount != 1 || failedKind != "" {
			t.Fatalf("attempt 1: retried=%d failedKind=%q, want 1 and empty", retriedCount, failedKind)
		}

		// Attempt 3 -> terminal retryable failure
		if err := s.processClaimedWithLease(context.Background(), &appdb.IngestionJob{
			ID: 3, Bucket: "b", ObjectKey: "incoming/ok.jpg", MediaClass: appdb.IngestionMediaClassImage,
			Attempts: 3, LeaseToken: "tok3",
		}); err != nil {
			t.Fatalf("attempt 3: %v", err)
		}
		if failedKind != appdb.IngestionFailureRetryable {
			t.Fatalf("attempt 3: failedKind=%q, want %q", failedKind, appdb.IngestionFailureRetryable)
		}
	})
}

func TestIndependentImageAndVideoWorkersAvoidHeadOfLineBlocking(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	validJPEG := []byte{0xFF, 0xD8, 0xFF, 0xE0, 0x00, 0x10}

	var mu sync.Mutex
	videoQueue := []*appdb.IngestionJob{
		{ID: 10, Bucket: "b", ObjectKey: "incoming/long.mp4", MediaClass: appdb.IngestionMediaClassVideo, Attempts: 1, LeaseToken: "vtok"},
	}
	imageQueue := []*appdb.IngestionJob{
		{ID: 11, Bucket: "b", ObjectKey: "incoming/a.jpg", MediaClass: appdb.IngestionMediaClassImage, Attempts: 1, LeaseToken: "itok1"},
		{ID: 12, Bucket: "b", ObjectKey: "incoming/b.jpg", MediaClass: appdb.IngestionMediaClassImage, Attempts: 1, LeaseToken: "itok2"},
	}

	videoStarted := make(chan struct{})
	releaseVideo := make(chan struct{})
	imagesCompleted := make(chan int64, 2)

	s := &Service{
		imageWorkers:  1,
		videoWorkers:  1,
		maxImageBytes: 1024,
		leaseDuration: time.Minute,
		store: stubFetcher{get: func(context.Context, string, string, string) (storage.Object, error) {
			return storage.Object{Body: io.NopCloser(bytes.NewReader(validJPEG)), ContentType: "image/jpeg"}, nil
		}},
		embedder: stubEmbedder{embed: func(context.Context, string, string, io.Reader) (embedder.Result, error) {
			return embedder.Result{Values: []float64{1}, Model: appdb.DefaultEmbeddingModel}, nil
		}},
		videoHandler: func(ctx context.Context, _ *appdb.IngestionJob) (string, string, error) {
			close(videoStarted)
			select {
			case <-releaseVideo:
				return appdb.IngestionJobIgnored, "done", nil
			case <-ctx.Done():
				return "", "", ctx.Err()
			}
		},
		claimByClass: func(_ context.Context, mediaClass string) (*appdb.IngestionJob, error) {
			mu.Lock()
			defer mu.Unlock()
			switch mediaClass {
			case appdb.IngestionMediaClassVideo:
				if len(videoQueue) == 0 {
					return nil, nil
				}
				job := videoQueue[0]
				videoQueue = videoQueue[1:]
				return job, nil
			case appdb.IngestionMediaClassImage:
				if len(imageQueue) == 0 {
					return nil, nil
				}
				job := imageQueue[0]
				imageQueue = imageQueue[1:]
				return job, nil
			default:
				return nil, nil
			}
		},
		heartbeatJob:  func(context.Context, int64, string) error { return nil },
		saveEmbedding: func(context.Context, appdb.SaveEmbeddingInput) error { return nil },
		completeJob: func(_ context.Context, id int64, _ string, status, _ string) error {
			if status == appdb.IngestionJobCompleted {
				imagesCompleted <- id
			}
			return nil
		},
		failJob:  func(context.Context, int64, string, string, string) error { return nil },
		retryJob: func(context.Context, int64, string, time.Time, string) error { return nil },
	}

	done := make(chan error, 1)
	go func() {
		done <- s.Run(ctx, 10*time.Millisecond)
	}()

	// Wait until the video worker is actively blocked inside the slow video job.
	select {
	case <-videoStarted:
	case <-time.After(time.Second):
		t.Fatal("video worker did not start")
	}

	// Both image jobs MUST complete while the video job is still blocked!
	for range 2 {
		select {
		case <-imagesCompleted:
		case <-time.After(time.Second):
			t.Fatal("image worker was head-of-line blocked by the active video job")
		}
	}

	close(releaseVideo)
	cancel()
	if err := <-done; err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
}

func TestRunDrainsReadyJobsBeforePolling(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()

	calls := 0
	service := &Service{
		processNext: func(context.Context) (bool, error) {
			calls++
			if calls == 3 {
				cancel()
				return false, nil
			}
			return true, nil
		},
	}

	done := make(chan error, 1)
	go func() {
		done <- service.Run(ctx, time.Hour)
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("run: %v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("worker waited for the poll interval instead of draining ready jobs")
	}
	if calls != 3 {
		t.Fatalf("process calls = %d, want 3", calls)
	}
}
