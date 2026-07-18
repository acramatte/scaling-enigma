package database

import (
	"context"
	"fmt"
	"strings"
	"time"

	"gorm.io/gorm"
	"gorm.io/gorm/clause"
)

type EnqueueIngestionJobInput struct {
	Bucket        string
	ObjectKey     string
	ObjectVersion string
	ETag          string
	SizeBytes     *int64
	ContentType   string
}

// EnqueueIngestionJob records an object event once and reports whether a new
// row was inserted. Repeated notifications for the same identity are ignored.
func EnqueueIngestionJob(ctx context.Context, db *gorm.DB, input EnqueueIngestionJobInput) (bool, error) {
	if strings.TrimSpace(input.Bucket) == "" || strings.TrimSpace(input.ObjectKey) == "" {
		return false, fmt.Errorf("ingestion job bucket and object key are required")
	}
	if input.SizeBytes != nil && *input.SizeBytes < 0 {
		return false, fmt.Errorf("ingestion job size must be non-negative")
	}

	job := IngestionJob{
		Bucket:        strings.TrimSpace(input.Bucket),
		ObjectKey:     strings.TrimSpace(input.ObjectKey),
		ObjectVersion: strings.TrimSpace(input.ObjectVersion),
		ETag:          strings.Trim(strings.TrimSpace(input.ETag), "\""),
		SizeBytes:     input.SizeBytes,
		ContentType:   strings.TrimSpace(input.ContentType),
		Status:        IngestionJobPending,
		AvailableAt:   time.Now().UTC(),
	}

	result := db.WithContext(ctx).Clauses(clause.OnConflict{DoNothing: true}).Create(&job)
	if result.Error != nil {
		return false, result.Error
	}
	return result.RowsAffected == 1, nil
}

// RequeueFailedIngestionJob explicitly gives a terminal failed job a fresh set
// of worker attempts. Other states are left unchanged so this operation cannot
// race a pending or processing job.
func RequeueFailedIngestionJob(ctx context.Context, db *gorm.DB, id int64) error {
	if id <= 0 {
		return fmt.Errorf("ingestion job ID must be positive")
	}
	now := time.Now().UTC()
	result := db.WithContext(ctx).Model(&IngestionJob{}).
		Where("id = ? AND status = ?", id, IngestionJobFailed).
		Updates(map[string]any{
			"status":       IngestionJobPending,
			"attempts":     0,
			"last_error":   "",
			"available_at": now,
			"started_at":   nil,
			"completed_at": nil,
			"updated_at":   now,
		})
	if result.Error != nil {
		return fmt.Errorf("requeue failed ingestion job: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("ingestion job %d does not exist or is not failed", id)
	}
	return nil
}

// ClaimIngestionJob obtains one ready job without competing workers processing
// the same record. A nil job means there is no work available.
func ClaimIngestionJob(ctx context.Context, db *gorm.DB) (*IngestionJob, error) {
	var job IngestionJob
	err := db.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		result := tx.Clauses(clause.Locking{Strength: "UPDATE", Options: "SKIP LOCKED"}).
			Where("status = ? AND available_at <= ?", IngestionJobPending, time.Now().UTC()).
			Order("available_at, id").
			Limit(1).
			Find(&job)
		if result.Error != nil {
			return result.Error
		}
		if result.RowsAffected == 0 {
			return nil
		}

		now := time.Now().UTC()
		return tx.Model(&job).Updates(map[string]any{
			"status":     IngestionJobProcessing,
			"attempts":   gorm.Expr("attempts + 1"),
			"started_at": now,
			"updated_at": now,
		}).Error
	})
	if err != nil {
		return nil, fmt.Errorf("claim ingestion job: %w", err)
	}
	if job.ID == 0 {
		return nil, nil
	}
	job.Status = IngestionJobProcessing
	job.Attempts++
	return &job, nil
}

func CompleteIngestionJob(ctx context.Context, db *gorm.DB, id int64, status string, reason string) error {
	if status != IngestionJobCompleted && status != IngestionJobIgnored && status != IngestionJobFailed {
		return fmt.Errorf("invalid terminal ingestion job status %q", status)
	}
	now := time.Now().UTC()
	result := db.WithContext(ctx).Model(&IngestionJob{}).
		Where("id = ? AND status = ?", id, IngestionJobProcessing).
		Updates(map[string]any{
			"status":       status,
			"last_error":   reason,
			"completed_at": now,
			"updated_at":   now,
		})
	if result.Error != nil {
		return fmt.Errorf("complete ingestion job: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("ingestion job %d is not being processed", id)
	}
	return nil
}

// RetryIngestionJob puts a processing job back into the ready queue after a
// transient failure. Attempts are incremented when the job is claimed.
func RetryIngestionJob(ctx context.Context, db *gorm.DB, id int64, availableAt time.Time, reason string) error {
	result := db.WithContext(ctx).Model(&IngestionJob{}).
		Where("id = ? AND status = ?", id, IngestionJobProcessing).
		Updates(map[string]any{
			"status":       IngestionJobPending,
			"available_at": availableAt.UTC(),
			"last_error":   reason,
			"updated_at":   time.Now().UTC(),
		})
	if result.Error != nil {
		return fmt.Errorf("retry ingestion job: %w", result.Error)
	}
	if result.RowsAffected != 1 {
		return fmt.Errorf("ingestion job %d is not being processed", id)
	}
	return nil
}
