package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"mime/multipart"
	"net/http"
	"os"
	"path/filepath"
	"time"
)

type EmbeddingResponse struct {
	Embedding []float64 `json:"embedding"`
}

// Client wraps the local FastAPI connection
type EmbedderClient struct {
	endpoint string
	client   *http.Client
}

func NewEmbedderClient(endpoint string) *EmbedderClient {
	return &EmbedderClient{
		endpoint: endpoint,
		client: &http.Client{
			Timeout: 10 * time.Second,
		},
	}
}

func previewLength(limit int, values []float64) int {
	if len(values) < limit {
		return len(values)
	}
	return limit
}

// GetFrameEmbedding sends raw image data directly to the NPU service
func (c *EmbedderClient) GetFrameEmbedding(imagePath string) ([]float64, error) {
	file, err := os.Open(imagePath)
	if err != nil {
		return nil, fmt.Errorf("failed to open file: %w", err)
	}
	defer file.Close()

	// Prepare multipart form data
	body := &bytes.Buffer{}
	writer := multipart.NewWriter(body)
	part, err := writer.CreateFormFile("file", filepath.Base(imagePath))
	if err != nil {
		return nil, fmt.Errorf("failed to create form file: %w", err)
	}

	_, err = io.Copy(part, file)
	if err != nil {
		return nil, fmt.Errorf("failed to copy file payload: %w", err)
	}
	if err := writer.Close(); err != nil {
		return nil, fmt.Errorf("failed to close multipart writer: %w", err)
	}

	// Request FastAPI local service
	req, err := http.NewRequest("POST", c.endpoint+"/embed", body)
	if err != nil {
		return nil, fmt.Errorf("failed to construct request: %w", err)
	}
	req.Header.Set("Content-Type", writer.FormDataContentType())

	resp, err := c.client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("network call to NPU service failed: %w", err)
	}
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusOK {
		respBody, _ := io.ReadAll(resp.Body)
		return nil, fmt.Errorf("NPU service returned error (%d): %s", resp.StatusCode, string(respBody))
	}

	var response EmbeddingResponse
	if err := json.NewDecoder(resp.Body).Decode(&response); err != nil {
		return nil, fmt.Errorf("failed parsing embedding array: %w", err)
	}

	return response.Embedding, nil
}

func main() {
	if len(os.Args) != 2 {
		fmt.Fprintf(os.Stderr, "usage: go run client.go /path/to/image\n")
		os.Exit(2)
	}

	// Initialize Go client targeting localhost
	client := NewEmbedderClient("http://127.0.0.1:8000")
	imagePath := os.Args[1]

	fmt.Printf("Generating embedding for %s...\n", imagePath)
	startTime := time.Now()

	vector, err := client.GetFrameEmbedding(imagePath)
	if err != nil {
		fmt.Fprintf(os.Stderr, "Error: %v\n", err)
		os.Exit(1)
	}

	duration := time.Since(startTime)
	preview := previewLength(5, vector)

	fmt.Printf("Successfully generated vector in %v!\n", duration)
	fmt.Printf("Vector Dimensions: %d\n", len(vector))
	fmt.Printf("First %d Dimensions: %v\n", preview, vector[:preview])
}
