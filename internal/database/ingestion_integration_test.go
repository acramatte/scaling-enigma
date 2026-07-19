//go:build integration

package database

import (
	"context"
	"testing"
	"time"
)

func TestIngestionQueueIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store := openIntegrationStore(t, ctx)

	identity := EnqueueIngestionJobInput{
		Bucket:        "semantic-search",
		ObjectKey:     " incoming/photo.jpg ",
		ObjectVersion: "version-1",
		ETag:          `"etag-1"`,
		ContentType:   "image/jpeg",
	}
	inserted, err := store.EnqueueIngestionJob(ctx, identity)
	if err != nil {
		t.Fatalf("enqueue ingestion job: %v", err)
	}
	if !inserted {
		t.Fatal("first notification was not inserted")
	}
	inserted, err = store.EnqueueIngestionJob(ctx, identity)
	if err != nil {
		t.Fatalf("enqueue duplicate ingestion job: %v", err)
	}
	if inserted {
		t.Fatal("duplicate notification inserted another job")
	}

	job, err := store.ClaimIngestionJob(ctx)
	if err != nil {
		t.Fatalf("claim ingestion job: %v", err)
	}
	if job == nil {
		t.Fatal("expected a claimed ingestion job")
	}
	if job.ObjectKey != identity.ObjectKey {
		t.Fatalf("object key = %q, want exact event key %q", job.ObjectKey, identity.ObjectKey)
	}
	if job.ETag != "etag-1" {
		t.Fatalf("ETag = %q, want normalized value", job.ETag)
	}
	if job.Attempts != 1 {
		t.Fatalf("attempts = %d, want 1 after claim", job.Attempts)
	}
	if err := store.CompleteIngestionJob(ctx, job.ID, IngestionJobFailed, "test failure"); err != nil {
		t.Fatalf("fail ingestion job: %v", err)
	}
	if err := store.RequeueFailedIngestionJob(ctx, job.ID); err != nil {
		t.Fatalf("requeue failed ingestion job: %v", err)
	}
	retried, err := store.ClaimIngestionJob(ctx)
	if err != nil {
		t.Fatalf("claim requeued ingestion job: %v", err)
	}
	if retried == nil || retried.ID != job.ID || retried.Attempts != 1 {
		t.Fatalf("requeued job = %#v, want original ID with fresh attempt count", retried)
	}
	if err := store.CompleteIngestionJob(ctx, retried.ID, IngestionJobCompleted, ""); err != nil {
		t.Fatalf("complete requeued ingestion job: %v", err)
	}
	if err := store.RequeueFailedIngestionJob(ctx, retried.ID); err == nil {
		t.Fatal("completed job was incorrectly requeued")
	}

	transientInserted, err := store.EnqueueIngestionJob(ctx, EnqueueIngestionJobInput{
		Bucket:    "semantic-search",
		ObjectKey: "incoming/transient.jpg",
		ETag:      "transient-etag",
	})
	if err != nil || !transientInserted {
		t.Fatalf("enqueue transient job: inserted=%t err=%v", transientInserted, err)
	}
	transient, err := store.ClaimIngestionJob(ctx)
	if err != nil {
		t.Fatalf("claim transient job: %v", err)
	}
	if transient == nil {
		t.Fatal("expected transient job")
	}
	if err := store.RetryIngestionJob(ctx, transient.ID, time.Now().Add(time.Hour), "temporary embedder failure"); err != nil {
		t.Fatalf("retry transient job: %v", err)
	}
	unavailable, err := store.ClaimIngestionJob(ctx)
	if err != nil {
		t.Fatalf("claim before retry is available: %v", err)
	}
	if unavailable != nil {
		t.Fatalf("future retry was claimed early: %#v", unavailable)
	}
	if err := store.RetryIngestionJob(ctx, transient.ID, time.Now(), "not processing"); err == nil {
		t.Fatal("pending retry job was incorrectly retried again")
	}
	if _, err := store.pool.Exec(ctx, "UPDATE ingestion_jobs SET available_at = CURRENT_TIMESTAMP WHERE id = $1", transient.ID); err != nil {
		t.Fatalf("make transient retry available: %v", err)
	}
	available, err := store.ClaimIngestionJob(ctx)
	if err != nil {
		t.Fatalf("claim available retry: %v", err)
	}
	if available == nil || available.ID != transient.ID {
		t.Fatalf("available retry = %#v, want original transient job", available)
	}
	if available.Attempts != 2 {
		t.Fatalf("retry attempts = %d, want 2", available.Attempts)
	}
	if available.LastError != "temporary embedder failure" {
		t.Fatalf("retry reason = %q, want persisted transient failure", available.LastError)
	}
	if err := store.CompleteIngestionJob(ctx, available.ID, IngestionJobCompleted, ""); err != nil {
		t.Fatalf("complete available retry: %v", err)
	}

	for _, key := range []string{"incoming/concurrent-a.jpg", "incoming/concurrent-b.jpg"} {
		inserted, err := store.EnqueueIngestionJob(ctx, EnqueueIngestionJobInput{
			Bucket:    "semantic-search",
			ObjectKey: key,
			ETag:      key,
		})
		if err != nil || !inserted {
			t.Fatalf("enqueue concurrent job %q: inserted=%t err=%v", key, inserted, err)
		}
	}

	type claimResult struct {
		job *IngestionJob
		err error
	}
	claims := make(chan claimResult, 2)
	for range 2 {
		go func() {
			claimed, err := store.ClaimIngestionJob(ctx)
			claims <- claimResult{job: claimed, err: err}
		}()
	}
	claimedIDs := make(map[int64]struct{}, 2)
	for range 2 {
		result := <-claims
		if result.err != nil {
			t.Fatalf("concurrent claim: %v", result.err)
		}
		if result.job == nil {
			t.Fatal("concurrent claim returned no job")
		}
		if _, exists := claimedIDs[result.job.ID]; exists {
			t.Fatalf("job %d was claimed by two workers", result.job.ID)
		}
		claimedIDs[result.job.ID] = struct{}{}
		if err := store.CompleteIngestionJob(ctx, result.job.ID, IngestionJobCompleted, ""); err != nil {
			t.Fatalf("complete concurrently claimed job: %v", err)
		}
	}

	empty, err := store.ClaimIngestionJob(ctx)
	if err != nil {
		t.Fatalf("claim empty queue: %v", err)
	}
	if empty != nil {
		t.Fatalf("empty queue returned job %#v", empty)
	}
}
