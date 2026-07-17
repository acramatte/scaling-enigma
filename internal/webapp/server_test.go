package webapp

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	appdb "semantic-search/internal/database"
)

func TestSearchPage(t *testing.T) {
	var receivedQuery string
	handler := newHandler(func(_ context.Context, query string) ([]appdb.SearchResult, error) {
		receivedQuery = query
		start := int64(1_000)
		end := int64(2_500)
		return []appdb.SearchResult{
			{
				SourceURI:    "s3://media/<sunset>.jpg",
				MediaType:    appdb.MediaTypeVideo,
				SegmentIndex: 3,
				StartMS:      &start,
				EndMS:        &end,
				Similarity:   0.91234,
			},
		}, nil
	})

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
		"s3://media/&lt;sunset&gt;.jpg",
		"Segment 3 · 1s–2.5s",
		"cosine 0.9123",
	} {
		if !strings.Contains(body, expected) {
			t.Errorf("response does not contain %q", expected)
		}
	}
	if strings.Contains(body, "s3://media/<sunset>.jpg") {
		t.Error("source URI was not HTML-escaped")
	}
}

func TestEmptySearchDoesNotCallSearcher(t *testing.T) {
	handler := newHandler(func(context.Context, string) ([]appdb.SearchResult, error) {
		t.Fatal("searcher called for an empty query")
		return nil, nil
	})

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
	})
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
