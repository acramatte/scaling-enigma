package embedder

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type embeddingResponse struct {
	Embedding []float64 `json:"embedding"`
	Model     string    `json:"model"`
}

type Result struct {
	Values []float64
	Model  string
}

// Client wraps the FastAPI embedding service connection.
type Client struct {
	endpoint string
	client   *http.Client
}

func NewClient(endpoint string) *Client {
	return &Client{
		endpoint: strings.TrimRight(endpoint, "/"),
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

// GetFrameEmbedding sends raw image data directly to the embedding service.
func (c *Client) GetFrameEmbedding(imagePath string) (Result, error) {
	file, err := os.Open(imagePath)
	if err != nil {
		return Result{}, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	return c.GetImageEmbeddingContext(context.Background(), filepath.Base(imagePath), "", file)
}

// GetImageEmbeddingContext sends image data to the embedding service while
// allowing callers to cancel the HTTP request.
func (c *Client) GetImageEmbeddingContext(ctx context.Context, filename, contentType string, image io.Reader) (Result, error) {
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		return Result{}, fmt.Errorf("failed to create form file: %w", err)
	}

	_, err = io.Copy(part, image)
	if err != nil {
		return Result{}, fmt.Errorf("failed to copy file payload: %w", err)
	}
	if err := writer.Close(); err != nil {
		return Result{}, fmt.Errorf("failed to close multipart writer: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint+"/embed/image", body)
	if err != nil {
		return Result{}, fmt.Errorf("failed to construct request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	return c.do(req)
}

// GetTextEmbedding embeds a natural-language query in the same vector space as
// the image embeddings returned by GetFrameEmbedding.
func (c *Client) GetTextEmbedding(text string) (Result, error) {
	return c.GetTextEmbeddingContext(context.Background(), text)
}

// GetTextEmbeddingContext embeds text while allowing callers to cancel the
// request, for example when an incoming HTTP request is abandoned.
func (c *Client) GetTextEmbeddingContext(ctx context.Context, text string) (Result, error) {
	payload, err := json.Marshal(struct {
		Text string `json:"text"`
	}{Text: text})
	if err != nil {
		return Result{}, fmt.Errorf("failed to encode text embedding request: %w", err)
	}

	req, err := http.NewRequestWithContext(ctx, "POST", c.endpoint+"/embed/text", bytes.NewReader(payload))
	if err != nil {
		return Result{}, fmt.Errorf("failed to construct request: %w", err)
	}
	req.Header.Set("Content-Type", "application/json")

	return c.do(req)
}

func (c *Client) do(req *http.Request) (Result, error) {
	resp, err := c.client.Do(req)
	if err != nil {
		return Result{}, fmt.Errorf("network call to embedding service failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return Result{}, fmt.Errorf("embedding service returned error (%d): %s", resp.StatusCode, string(respBody))
	}

	var response embeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return Result{}, fmt.Errorf("failed parsing embedding array: %w", err)
	}

	return Result{Values: response.Embedding, Model: response.Model}, nil
}
