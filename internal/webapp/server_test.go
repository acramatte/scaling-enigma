package webapp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strings"
	"testing"

	appdb "semantic-search/internal/database"
	"semantic-search/internal/storage"
)

func TestSearchPage(t *testing.T) {
	imageBytes := []byte{0xff, 0xd8, 0xff, 0xd9}
	store := testImageStore(t, imageBytes, "image/jpeg")
	imageURI := "s3://semantic-search/incoming/sunset.jpg"

	var receivedQuery string
	handler := newHandler(func(_ context.Context, query string) ([]appdb.SearchResult, error) {
		receivedQuery = query
		start := int64(1_000)
		end := int64(2_500)
		return []appdb.SearchResult{
			{
				SourceURI:    imageURI,
				MediaType:    appdb.MediaTypeImage,
				SegmentIndex: 3,
				StartMS:      &start,
				EndMS:        &end,
				Similarity:   0.91234,
			},
		}, nil
	}, store)

	request := httptest.NewRequest(http.MethodGet, "/?q=++sunset+over+mountains++", nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if receivedQuery != "sunset over mountains" {
		t.Fatalf("query = %q, want trimmed query", receivedQuery)
	}

	body := response.Body.String()
	for _, expected := range []string{
		"sunset over mountains",
		"s3://semantic-search/incoming/sunset.jpg",
		"class=\"thumbnail\"",
		"Segment 3 · 1s–2.5s",
		"cosine 0.9123",
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("response does not contain %q", expected)
		}
	}

	imageURL := extractImageURL(t, body)
	imageResponse := httptest.NewRecorder()
	handler.ServeHTTP(imageResponse, httptest.NewRequest(http.MethodGet, imageURL, nil))
	if imageResponse.Code != http.StatusOK {
		t.Fatalf("image status = %d, want %d", imageResponse.Code, http.StatusOK)
	}
	if contentType := imageResponse.Header().Get("Content-Type"); contentType != "image/jpeg" {
		t.Fatalf("image Content-Type = %q, want image/jpeg", contentType)
	}
	if got := imageResponse.Body.Bytes(); string(got) != string(imageBytes) {
		t.Fatalf("image bytes = %v, want %v", got, imageBytes)
	}
}

func TestFileResultDoesNotRenderPreview(t *testing.T) {
	handler := newHandler(func(context.Context, string) ([]appdb.SearchResult, error) {
		return []appdb.SearchResult{{
			SourceURI:  "file:///tmp/sunset.jpg",
			MediaType:  appdb.MediaTypeImage,
			Similarity: 0.9,
		}}, nil
	}, nil)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/?q=sunset", nil))
	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	body := response.Body.String()
	if strings.Contains(body, "class=\"thumbnail\"") {
		t.Fatalf("file result unexpectedly rendered an image preview:\n%s", body)
	}
	if !strings.Contains(body, "No preview") {
		t.Fatalf("file result did not render the preview placeholder:\n%s", body)
	}
}

func TestEmptySearchDoesNotCallSearcher(t *testing.T) {
	handler := newHandler(func(context.Context, string) ([]appdb.SearchResult, error) {
		t.Fatal("searcher called for an empty query")
		return nil, nil
	}, nil)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/", nil))

	if response.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusOK)
	}
	if !strings.Contains(response.Body.String(), "Find an image by describing it") {
		t.Error("landing page heading is missing")
	}
}

func TestSearchPageRejectsUnsupportedMethod(t *testing.T) {
	handler := newHandler(func(context.Context, string) ([]appdb.SearchResult, error) {
		return nil, nil
	}, nil)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/", nil))

	result := response.Result()
	defer result.Body.Close()
	_, _ = io.Copy(io.Discard, result.Body)
	if result.StatusCode != http.StatusMethodNotAllowed {
		t.Fatalf("status = %d, want %d", result.StatusCode, http.StatusMethodNotAllowed)
	}
	if result.Header.Get("Allow") != http.MethodGet {
		t.Fatalf("Allow = %q, want GET", result.Header.Get("Allow"))
	}
}

func TestImageEndpointRejectsUnsignedSources(t *testing.T) {
	handler := newHandler(func(context.Context, string) ([]appdb.SearchResult, error) {
		return nil, nil
	}, nil)

	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, "/image?src=s3://semantic-search/incoming/test.jpg", nil))
	if response.Code != http.StatusNotFound {
		t.Fatalf("status = %d, want %d", response.Code, http.StatusNotFound)
	}
}

func testImageStore(t *testing.T, imageBytes []byte, contentType string) *storage.Store {
	t.Helper()
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path != "/semantic-search/incoming/sunset.jpg" {
			t.Errorf("unexpected object path: %s", r.URL.Path)
		}
		w.Header().Set("Content-Type", contentType)
		_, _ = w.Write(imageBytes)
	}))
	t.Cleanup(server.Close)

	t.Setenv("S3_ENDPOINT", server.URL)
	t.Setenv("S3_REGION", "us-east-1")
	t.Setenv("S3_ACCESS_KEY", "test-access-key")
	t.Setenv("S3_SECRET_KEY", "test-secret-key")
	store, err := storage.NewFromEnvironment(context.Background())
	if err != nil {
		t.Fatalf("create image store: %v", err)
	}
	return store
}

func extractImageURL(t *testing.T, body string) string {
	t.Helper()
	match := regexp.MustCompile(`<img class="thumbnail" src="([^"]+)"`).FindStringSubmatch(body)
	if match == nil {
		t.Fatalf("thumbnail URL not found in response:\n%s", body)
	}
	return strings.ReplaceAll(match[1], "&amp;", "&")
}
