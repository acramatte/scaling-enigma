package ingestion

import (
	"context"
	"encoding/json/v2"
	"fmt"
	"log"
	"mime"
	"net/http"
	"net/url"
	"path"
	"strings"
	"time"

	appdb "semantic-search/internal/database"
	"semantic-search/internal/embedder"
	"semantic-search/internal/storage"
)

type Service struct {
	db          *appdb.Store
	store       *storage.Store
	embedder    *embedder.Client
	token       string
	enqueue     func(context.Context, appdb.EnqueueIngestionJobInput) (bool, error)
	processNext func(context.Context) (bool, error)
}

const maxAttempts = 3

type eventNotification struct {
	Records []eventRecord `json:"Records"`
}

type eventRecord struct {
	EventName string `json:"eventName"`
	S3        struct {
		Bucket struct {
			Name string `json:"name"`
		} `json:"bucket"`
		Object struct {
			Key         string `json:"key"`
			VersionID   string `json:"versionId"`
			ETag        string `json:"eTag"`
			Size        int64  `json:"size"`
			ContentType string `json:"contentType"`
		} `json:"object"`
	} `json:"s3"`
}

func New(db *appdb.Store, store *storage.Store, client *embedder.Client, webhookToken string) *Service {
	return &Service{
		db:       db,
		store:    store,
		embedder: client,
		token:    strings.TrimSpace(webhookToken),
		enqueue: func(ctx context.Context, input appdb.EnqueueIngestionJobInput) (bool, error) {
			return db.EnqueueIngestionJob(ctx, input)
		},
	}
}

// HandleEvent accepts S3-compatible ObjectCreated notifications. It
// only records jobs; slow object fetching and embedding are performed by Run.
func (s *Service) HandleEvent(w http.ResponseWriter, r *http.Request) {
	log.Printf("S3 webhook request received remote=%q content_length=%d", r.RemoteAddr, r.ContentLength)
	if r.Method != http.MethodPost {
		log.Printf("S3 webhook request rejected remote=%q reason=method_not_allowed method=%q", r.RemoteAddr, r.Method)
		w.Header().Set("Allow", http.MethodPost)
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if s.token != "" && r.Header.Get("Authorization") != s.token && r.Header.Get("Authorization") != "Bearer "+s.token {
		log.Printf("S3 webhook request rejected remote=%q reason=unauthorized", r.RemoteAddr)
		http.Error(w, "unauthorized", http.StatusUnauthorized)
		return
	}

	defer r.Body.Close()
	var notification eventNotification
	limited := http.MaxBytesReader(w, r.Body, 1<<20)
	if err := json.UnmarshalRead(limited, &notification); err != nil {
		log.Printf("S3 webhook request rejected remote=%q reason=invalid_notification error=%q", r.RemoteAddr, err)
		http.Error(w, "invalid S3 event notification", http.StatusBadRequest)
		return
	}

	accepted := 0
	for _, record := range notification.Records {
		if !strings.HasPrefix(record.EventName, "s3:ObjectCreated:") {
			continue
		}
		key, err := url.QueryUnescape(record.S3.Object.Key)
		if err != nil {
			log.Printf("S3 webhook request rejected remote=%q reason=invalid_object_key key=%q error=%q", r.RemoteAddr, record.S3.Object.Key, err)
			http.Error(w, "invalid S3 object key", http.StatusBadRequest)
			return
		}
		size := record.S3.Object.Size
		inserted, err := s.enqueue(r.Context(), appdb.EnqueueIngestionJobInput{
			Bucket:        record.S3.Bucket.Name,
			ObjectKey:     key,
			ObjectVersion: record.S3.Object.VersionID,
			ETag:          record.S3.Object.ETag,
			SizeBytes:     &size,
			ContentType:   record.S3.Object.ContentType,
		})
		if err != nil {
			log.Printf("S3 webhook object failed bucket=%q key=%q event=%q error=%q", record.S3.Bucket.Name, key, record.EventName, err)
			http.Error(w, "could not record ingestion job", http.StatusInternalServerError)
			return
		}
		accepted++
		if inserted {
			log.Printf("S3 webhook object inserted bucket=%q key=%q event=%q etag=%q", record.S3.Bucket.Name, key, record.EventName, record.S3.Object.ETag)
		} else {
			log.Printf("S3 webhook object duplicate bucket=%q key=%q event=%q etag=%q", record.S3.Bucket.Name, key, record.EventName, record.S3.Object.ETag)
		}
	}
	log.Printf("S3 webhook request completed remote=%q records=%d accepted=%d", r.RemoteAddr, len(notification.Records), accepted)
	w.WriteHeader(http.StatusNoContent)
}

func newEventService(token string, enqueue func(context.Context, appdb.EnqueueIngestionJobInput) (bool, error)) *Service {
	return &Service{token: token, enqueue: enqueue}
}

// Run polls the durable job queue until ctx is canceled.
func (s *Service) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		processed, err := s.ProcessNext(ctx)
		if err != nil {
			return err
		}
		if processed {
			continue
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (s *Service) ProcessNext(ctx context.Context) (bool, error) {
	if s.processNext != nil {
		return s.processNext(ctx)
	}
	job, err := s.db.ClaimIngestionJob(ctx)
	if err != nil || job == nil {
		return false, err
	}
	return true, s.processClaimedWithLease(ctx, job)
}

func (s *Service) processClaimedWithLease(ctx context.Context, job *appdb.IngestionJob) error {
	jobCtx, cancel := context.WithCancel(ctx)
	defer cancel()
	heartbeatDone := make(chan struct{})
	heartbeatErr := make(chan error, 1)
	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(appdb.DefaultIngestionJobLeaseDuration / 3)
		defer ticker.Stop()
		for {
			select {
			case <-jobCtx.Done():
				return
			case <-ticker.C:
				if err := s.db.HeartbeatIngestionJob(jobCtx, job.ID, job.LeaseToken); err != nil {
					select {
					case heartbeatErr <- err:
					default:
					}
					cancel()
					return
				}
			}
		}
	}()

	status, reason, err := s.processClaimed(jobCtx, job)
	cancel()
	<-heartbeatDone
	select {
	case err := <-heartbeatErr:
		return fmt.Errorf("heartbeat ingestion job %d: %w", job.ID, err)
	default:
	}
	if err != nil {
		return s.fail(ctx, job, err)
	}
	return s.db.CompleteIngestionJob(ctx, job.ID, job.LeaseToken, status, reason)
}

func (s *Service) processClaimed(ctx context.Context, job *appdb.IngestionJob) (string, string, error) {
	if !isImage(job.ContentType, job.ObjectKey) {
		return appdb.IngestionJobIgnored, "only image ingestion is implemented", nil
	}

	object, err := s.store.Get(ctx, job.Bucket, job.ObjectKey, job.ObjectVersion)
	if err != nil {
		return "", "", err
	}
	defer object.Body.Close()
	contentType := job.ContentType
	if contentType == "" {
		contentType = object.ContentType
	}
	embedding, err := s.embedder.GetImageEmbeddingContext(ctx, path.Base(job.ObjectKey), contentType, object.Body)
	if err != nil {
		return "", "", fmt.Errorf("embed object: %w", err)
	}

	sourceURI := (&url.URL{Scheme: "s3", Host: job.Bucket, Path: "/" + job.ObjectKey}).String()
	if err := s.db.SaveDocumentEmbedding(ctx, appdb.SaveEmbeddingInput{
		SourceURI:   sourceURI,
		MediaType:   appdb.MediaTypeImage,
		ContentType: contentType,
		Model:       embedding.Model,
		DocumentMetadata: appdb.Metadata{
			"bucket":         job.Bucket,
			"object_key":     job.ObjectKey,
			"object_version": job.ObjectVersion,
			"etag":           job.ETag,
			"size_bytes":     job.SizeBytes,
		},
		EmbeddingMetadata: appdb.Metadata{"dimensions": len(embedding.Values)},
		Values:            embedding.Values,
	}); err != nil {
		return "", "", fmt.Errorf("save embedding: %w", err)
	}
	return appdb.IngestionJobCompleted, "", nil
}

func (s *Service) fail(ctx context.Context, job *appdb.IngestionJob, err error) error {
	if job.Attempts >= maxAttempts {
		return s.db.CompleteIngestionJob(ctx, job.ID, job.LeaseToken, appdb.IngestionJobFailed, err.Error())
	}
	backoff := time.Duration(job.Attempts) * time.Second
	return s.db.RetryIngestionJob(ctx, job.ID, job.LeaseToken, time.Now().Add(backoff), err.Error())
}

func isImage(contentType, objectKey string) bool {
	if strings.HasPrefix(strings.ToLower(contentType), "image/") {
		return true
	}
	return strings.HasPrefix(mime.TypeByExtension(path.Ext(objectKey)), "image/")
}
