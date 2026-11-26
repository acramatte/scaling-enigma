//go:build integration

package database

import (
	"context"
	"testing"
	"time"
)

func TestIngestionJobClassificationAndIsolationIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store := openIntegrationStore(t, ctx)

	// 1. Unsupported uploads are recorded immediately as ignored and never enter pending.
	inserted, err := store.EnqueueIngestionJob(ctx, EnqueueIngestionJobInput{
		Bucket:      "semantic-search",
		ObjectKey:   "incoming/notes.txt",
		ETag:        "notes-etag",
		ContentType: "text/plain",
	})
	if err != nil || !inserted {
		t.Fatalf("enqueue unsupported job: inserted=%t err=%v", inserted, err)
	}
	var unsupportedID int64
	if err := store.pool.QueryRow(ctx, "SELECT id FROM ingestion_jobs WHERE object_key = $1", "incoming/notes.txt").Scan(&unsupportedID); err != nil {
		t.Fatalf("query unsupported job ID: %v", err)
	}
	unsupportedJob, err := store.GetIngestionJob(ctx, unsupportedID)
	if err != nil {
		t.Fatalf("get unsupported job: %v", err)
	}
	if unsupportedJob.MediaClass != IngestionMediaClassUnsupported {
		t.Fatalf("unsupported media_class = %q, want %q", unsupportedJob.MediaClass, IngestionMediaClassUnsupported)
	}
	if unsupportedJob.Status != IngestionJobIgnored || unsupportedJob.CompletedAt == nil {
		t.Fatalf("unsupported job should be immediately ignored: %#v", unsupportedJob)
	}
	if unsupportedJob.FailureKind != IngestionFailureNone {
		t.Fatalf("unsupported failure_kind = %q, want empty", unsupportedJob.FailureKind)
	}
	// DB constraint must reject activating an unsupported job.
	if _, err := store.pool.Exec(ctx, "UPDATE ingestion_jobs SET status = 'pending', completed_at = NULL WHERE id = $1", unsupportedID); err == nil {
		t.Fatal("DB constraint failed to reject pending unsupported job")
	}

	// 2. Enqueue a video first, then an image; verify class-scoped claims isolate them.
	if inserted, err := store.EnqueueIngestionJob(ctx, EnqueueIngestionJobInput{
		Bucket:      "semantic-search",
		ObjectKey:   "incoming/clip.mp4",
		ETag:        "video-etag",
		ContentType: "video/mp4",
	}); err != nil || !inserted {
		t.Fatalf("enqueue video job: inserted=%t err=%v", inserted, err)
	}
	if inserted, err := store.EnqueueIngestionJob(ctx, EnqueueIngestionJobInput{
		Bucket:      "semantic-search",
		ObjectKey:   "incoming/photo.jpg",
		ETag:        "image-etag",
		ContentType: "image/jpeg",
	}); err != nil || !inserted {
		t.Fatalf("enqueue image job: inserted=%t err=%v", inserted, err)
	}

	// Image worker skips the older video job and claims the image directly.
	imgJob, err := store.ClaimIngestionJobByClass(ctx, IngestionMediaClassImage)
	if err != nil {
		t.Fatalf("claim image job: %v", err)
	}
	if imgJob == nil || imgJob.MediaClass != IngestionMediaClassImage || imgJob.ObjectKey != "incoming/photo.jpg" {
		t.Fatalf("image claim = %#v, want incoming/photo.jpg", imgJob)
	}

	// No more image jobs left while video is still pending.
	noMoreImages, err := store.ClaimIngestionJobByClass(ctx, IngestionMediaClassImage)
	if err != nil {
		t.Fatalf("second image claim: %v", err)
	}
	if noMoreImages != nil {
		t.Fatalf("image worker claimed non-image job: %#v", noMoreImages)
	}

	// Video worker claims the video job independently.
	vidJob, err := store.ClaimIngestionJobByClass(ctx, IngestionMediaClassVideo)
	if err != nil {
		t.Fatalf("claim video job: %v", err)
	}
	if vidJob == nil || vidJob.MediaClass != IngestionMediaClassVideo || vidJob.ObjectKey != "incoming/clip.mp4" {
		t.Fatalf("video claim = %#v, want incoming/clip.mp4", vidJob)
	}

	// 3. Failure kinds: permanent_media, retryable, and requeue cleanup.
	if err := store.FailIngestionJob(ctx, imgJob.ID, imgJob.LeaseToken, IngestionFailurePermanentMedia, "corrupt jpeg"); err != nil {
		t.Fatalf("fail image job permanently: %v", err)
	}
	failedImg, err := store.GetIngestionJob(ctx, imgJob.ID)
	if err != nil {
		t.Fatalf("get failed image job: %v", err)
	}
	if failedImg.Status != IngestionJobFailed || failedImg.FailureKind != IngestionFailurePermanentMedia {
		t.Fatalf("failed image = %#v, want status=failed failure_kind=permanent_media", failedImg)
	}

	// Requeue clears failure_kind and resets attempts.
	if err := store.RequeueFailedIngestionJob(ctx, imgJob.ID); err != nil {
		t.Fatalf("requeue failed image job: %v", err)
	}
	requeuedImg, err := store.GetIngestionJob(ctx, imgJob.ID)
	if err != nil {
		t.Fatalf("get requeued image job: %v", err)
	}
	if requeuedImg.Status != IngestionJobPending || requeuedImg.FailureKind != IngestionFailureNone || requeuedImg.Attempts != 0 {
		t.Fatalf("requeued image = %#v, want pending with empty failure_kind and attempts=0", requeuedImg)
	}

	// Retry persists failure_kind='retryable', and completing clears it.
	if err := store.RetryIngestionJob(ctx, vidJob.ID, vidJob.LeaseToken, time.Now().Add(-time.Second), "embedder 503"); err != nil {
		t.Fatalf("retry video job: %v", err)
	}
	retriedVid, err := store.GetIngestionJob(ctx, vidJob.ID)
	if err != nil {
		t.Fatalf("get retried video job: %v", err)
	}
	if retriedVid.Status != IngestionJobPending || retriedVid.FailureKind != IngestionFailureRetryable {
		t.Fatalf("retried video = %#v, want pending with failure_kind=retryable", retriedVid)
	}

	reclaimedVid, err := store.ClaimIngestionJobByClass(ctx, IngestionMediaClassVideo)
	if err != nil || reclaimedVid == nil {
		t.Fatalf("reclaim retried video: job=%#v err=%v", reclaimedVid, err)
	}
	if err := store.CompleteIngestionJob(ctx, reclaimedVid.ID, reclaimedVid.LeaseToken, IngestionJobIgnored, "video ingestion is not implemented yet"); err != nil {
		t.Fatalf("ignore video job: %v", err)
	}
	ignoredVid, err := store.GetIngestionJob(ctx, reclaimedVid.ID)
	if err != nil {
		t.Fatalf("get ignored video job: %v", err)
	}
	if ignoredVid.Status != IngestionJobIgnored || ignoredVid.FailureKind != IngestionFailureNone {
		t.Fatalf("ignored video = %#v, want ignored with cleared failure_kind", ignoredVid)
	}

	// DB constraint must reject non-empty failure_kind on ignored/completed rows.
	if _, err := store.pool.Exec(ctx, "UPDATE ingestion_jobs SET failure_kind = 'permanent_limit' WHERE id = $1", ignoredVid.ID); err == nil {
		t.Fatal("DB constraint failed to reject failure_kind on ignored job")
	}
}
