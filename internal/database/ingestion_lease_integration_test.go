//go:build integration

package database

import (
	"context"
	"testing"
	"time"
)

func TestIngestionJobLeaseFencingIntegration(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Minute)
	defer cancel()
	store := openIntegrationStore(t, ctx)

	inserted, err := store.EnqueueIngestionJob(ctx, EnqueueIngestionJobInput{
		Bucket:    "semantic-search",
		ObjectKey: "incoming/leased.jpg",
		ETag:      "leased-etag",
	})
	if err != nil || !inserted {
		t.Fatalf("enqueue leased job: inserted=%t err=%v", inserted, err)
	}

	first, err := store.ClaimIngestionJob(ctx)
	if err != nil {
		t.Fatalf("claim first lease: %v", err)
	}
	if first == nil || first.LeaseToken == "" || first.LeaseExpiresAt == nil {
		t.Fatalf("first claim missing lease state: %#v", first)
	}

	if _, err := store.pool.Exec(ctx, `
		UPDATE ingestion_jobs
		SET lease_expires_at = CURRENT_TIMESTAMP - INTERVAL '1 millisecond'
		WHERE id = $1
	`, first.ID); err != nil {
		t.Fatalf("expire first lease: %v", err)
	}
	if err := store.HeartbeatIngestionJob(ctx, first.ID, first.LeaseToken); err == nil {
		t.Fatal("expired lease heartbeat unexpectedly succeeded")
	}

	second, err := store.ClaimIngestionJob(ctx)
	if err != nil {
		t.Fatalf("reclaim expired job: %v", err)
	}
	if second == nil || second.ID != first.ID {
		t.Fatalf("reclaimed job = %#v, want job %d", second, first.ID)
	}
	if second.LeaseToken == first.LeaseToken || second.LeaseToken == "" {
		t.Fatalf("reclaimed lease token = %q, want a new non-empty token", second.LeaseToken)
	}
	if second.Attempts != first.Attempts+1 {
		t.Fatalf("reclaimed attempts = %d, want %d", second.Attempts, first.Attempts+1)
	}

	if err := store.CompleteIngestionJob(ctx, first.ID, first.LeaseToken, IngestionJobCompleted, ""); err == nil {
		t.Fatal("stale worker completed a reclaimed job")
	}
	if err := store.RetryIngestionJob(ctx, first.ID, first.LeaseToken, time.Now(), "stale retry"); err == nil {
		t.Fatal("stale worker retried a reclaimed job")
	}

	if err := store.HeartbeatIngestionJob(ctx, second.ID, second.LeaseToken); err != nil {
		t.Fatalf("heartbeat current lease: %v", err)
	}
	var leaseExpiresAt time.Time
	if err := store.pool.QueryRow(ctx, "SELECT lease_expires_at FROM ingestion_jobs WHERE id = $1", second.ID).Scan(&leaseExpiresAt); err != nil {
		t.Fatalf("read renewed lease expiry: %v", err)
	}
	if !leaseExpiresAt.After(time.Now()) {
		t.Fatalf("renewed lease expiry = %s, want a future timestamp", leaseExpiresAt)
	}

	claimed, err := store.ClaimIngestionJob(ctx)
	if err != nil {
		t.Fatalf("claim while current lease is active: %v", err)
	}
	if claimed != nil {
		t.Fatalf("active lease was reclaimed: %#v", claimed)
	}
	if err := store.CompleteIngestionJob(ctx, second.ID, second.LeaseToken, IngestionJobCompleted, ""); err != nil {
		t.Fatalf("complete current lease: %v", err)
	}
}
