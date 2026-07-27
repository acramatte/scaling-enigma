package database

import "time"

// EmbeddingDimensions must match both towers of the SigLIP 2 ONNX model.
// If the model changes, update this constant and the vector size in migrations.
const EmbeddingDimensions = 768
const DefaultEmbeddingModel = "google/siglip2-base-patch16-256"
const MediaTypeImage = "image"
const MediaTypeVideo = "video"
const MediaTypeUnknown = "unknown"
const DefaultIngestionJobLeaseDuration = 2 * time.Minute

const (
	IngestionJobPending    = "pending"
	IngestionJobProcessing = "processing"
	IngestionJobCompleted  = "completed"
	IngestionJobIgnored    = "ignored"
	IngestionJobFailed     = "failed"
)

type Metadata map[string]any

// IngestionJob is the domain representation of a single object version that
// must be fetched and indexed. The object identity makes at-least-once bucket
// notifications safe.
type IngestionJob struct {
	ID             int64
	Bucket         string
	ObjectKey      string
	ObjectVersion  string
	ETag           string
	SizeBytes      *int64
	ContentType    string
	Status         string
	Attempts       int
	LastError      string
	AvailableAt    time.Time
	StartedAt      *time.Time
	CompletedAt    *time.Time
	LeaseToken     string
	LeaseExpiresAt *time.Time
	HeartbeatAt    *time.Time
	CreatedAt      time.Time
	UpdatedAt      time.Time
}
