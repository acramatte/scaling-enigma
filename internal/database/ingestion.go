package database

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"semantic-search/internal/database/dbsql"

	"github.com/jackc/pgx/v5"
	"github.com/jackc/pgx/v5/pgtype"
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
func (s *Store) EnqueueIngestionJob(ctx context.Context, input EnqueueIngestionJobInput) (bool, error) {
	bucket := strings.TrimSpace(input.Bucket)
	if bucket == "" || strings.TrimSpace(input.ObjectKey) == "" {
		return false, fmt.Errorf("ingestion job bucket and object key are required")
	}
	if input.SizeBytes != nil && *input.SizeBytes < 0 {
		return false, fmt.Errorf("ingestion job size must be non-negative")
	}

	inserted, err := s.queries.EnqueueIngestionJob(ctx, dbsql.EnqueueIngestionJobParams{
		Bucket:        bucket,
		ObjectKey:     input.ObjectKey,
		ObjectVersion: strings.TrimSpace(input.ObjectVersion),
		Etag:          strings.Trim(strings.TrimSpace(input.ETag), `"`),
		SizeBytes:     input.SizeBytes,
		ContentType:   strings.TrimSpace(input.ContentType),
	})
	if err != nil {
		return false, fmt.Errorf("enqueue ingestion job: %w", err)
	}
	return inserted, nil
}

// RequeueFailedIngestionJob explicitly gives a terminal failed job a fresh set
// of worker attempts. Other states are left unchanged so this operation cannot
// race a pending or processing job.
func (s *Store) RequeueFailedIngestionJob(ctx context.Context, id int64) error {
	if id <= 0 {
		return fmt.Errorf("ingestion job ID must be positive")
	}
	if _, err := s.queries.RequeueFailedIngestionJob(ctx, id); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("ingestion job %d does not exist or is not failed", id)
		}
		return fmt.Errorf("requeue ingestion job %d: %w", id, err)
	}
	return nil
}

// ClaimIngestionJob atomically obtains and marks one ready job so competing
// workers cannot process the same record. A nil job means there is no work.
func (s *Store) ClaimIngestionJob(ctx context.Context) (*IngestionJob, error) {
	row, err := s.queries.ClaimIngestionJob(ctx)
	if errors.Is(err, pgx.ErrNoRows) {
		return nil, nil
	}
	if err != nil {
		return nil, fmt.Errorf("claim ingestion job: %w", err)
	}
	return ingestionJobFromRow(row), nil
}

func (s *Store) CompleteIngestionJob(ctx context.Context, id int64, status string, reason string) error {
	if status != IngestionJobCompleted && status != IngestionJobIgnored && status != IngestionJobFailed {
		return fmt.Errorf("invalid terminal ingestion job status %q", status)
	}
	if _, err := s.queries.CompleteIngestionJob(ctx, dbsql.CompleteIngestionJobParams{
		TerminalStatus: status,
		Reason:         reason,
		ID:             id,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("ingestion job %d was not processing", id)
		}
		return fmt.Errorf("complete ingestion job %d: %w", id, err)
	}
	return nil
}

// RetryIngestionJob puts a processing job back into the ready queue after a
// transient failure. Attempts are incremented when the job is claimed.
func (s *Store) RetryIngestionJob(ctx context.Context, id int64, availableAt time.Time, reason string) error {
	if _, err := s.queries.RetryIngestionJob(ctx, dbsql.RetryIngestionJobParams{
		AvailableAt: pgtype.Timestamptz{Time: availableAt.UTC(), Valid: true},
		Reason:      reason,
		ID:          id,
	}); err != nil {
		if errors.Is(err, pgx.ErrNoRows) {
			return fmt.Errorf("ingestion job %d was not processing", id)
		}
		return fmt.Errorf("retry ingestion job %d: %w", id, err)
	}
	return nil
}

func ingestionJobFromRow(row dbsql.IngestionJob) *IngestionJob {
	return &IngestionJob{
		ID:            row.ID,
		Bucket:        row.Bucket,
		ObjectKey:     row.ObjectKey,
		ObjectVersion: row.ObjectVersion,
		ETag:          row.Etag,
		SizeBytes:     row.SizeBytes,
		ContentType:   row.ContentType,
		Status:        row.Status,
		Attempts:      int(row.Attempts),
		LastError:     row.LastError,
		AvailableAt:   row.AvailableAt.Time,
		StartedAt:     timestampPointer(row.StartedAt),
		CompletedAt:   timestampPointer(row.CompletedAt),
		CreatedAt:     row.CreatedAt.Time,
		UpdatedAt:     row.UpdatedAt.Time,
	}
}

func timestampPointer(value pgtype.Timestamptz) *time.Time {
	if !value.Valid {
		return nil
	}
	timestamp := value.Time
	return &timestamp
}
