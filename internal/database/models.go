package database

import (
	"database/sql/driver"
	"encoding/json"
	"fmt"
	"time"

	"github.com/pgvector/pgvector-go"
)

// EmbeddingDimensions must match the output size of the SigLIP 2 vision ONNX model.
// If the model changes, update this constant and the vector size in migrations.
const EmbeddingDimensions = 768
const DefaultEmbeddingModel = "siglip2-vision"
const MediaTypeImage = "image"
const MediaTypeVideo = "video"
const MediaTypeUnknown = "unknown"

type Metadata map[string]any

func (m Metadata) Value() (driver.Value, error) {
	if m == nil {
		return "{}", nil
	}

	value, err := json.Marshal(m)
	if err != nil {
		return nil, fmt.Errorf("marshal metadata: %w", err)
	}

	return string(value), nil
}

func (m *Metadata) Scan(value any) error {
	if value == nil {
		*m = Metadata{}
		return nil
	}

	var data []byte
	switch typed := value.(type) {
	case []byte:
		data = typed
	case string:
		data = []byte(typed)
	default:
		return fmt.Errorf("scan metadata: unsupported type %T", value)
	}

	if len(data) == 0 {
		*m = Metadata{}
		return nil
	}

	if err := json.Unmarshal(data, m); err != nil {
		return fmt.Errorf("unmarshal metadata: %w", err)
	}

	return nil
}

type Document struct {
	ID          int64    `gorm:"primaryKey"`
	SourceURI   string   `gorm:"column:source_uri;not null;uniqueIndex"`
	MediaType   string   `gorm:"not null"`
	ContentType string   `gorm:"not null;default:''"`
	Metadata    Metadata `gorm:"type:jsonb;not null"`
	CreatedAt   time.Time
	UpdatedAt   time.Time
	Embeddings  []Embedding
}

func (Document) TableName() string {
	return "documents"
}

type Embedding struct {
	ID           int64           `gorm:"primaryKey"`
	DocumentID   int64           `gorm:"not null;uniqueIndex:idx_embedding_document_model_segment"`
	Document     *Document       `gorm:"foreignKey:DocumentID;constraint:OnDelete:CASCADE"`
	Model        string          `gorm:"not null;uniqueIndex:idx_embedding_document_model_segment"`
	SegmentIndex int             `gorm:"not null;default:0;uniqueIndex:idx_embedding_document_model_segment"`
	StartMS      *int64          `gorm:"column:start_ms"`
	EndMS        *int64          `gorm:"column:end_ms"`
	Vector       pgvector.Vector `gorm:"column:embedding;type:vector(768);not null"`
	Metadata     Metadata        `gorm:"type:jsonb;not null"`
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

func (Embedding) TableName() string {
	return "embeddings"
}
