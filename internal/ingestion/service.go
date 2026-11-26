package ingestion

import (
	"bytes"
	"context"
	"encoding/json/v2"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"path"
	"strings"
	"sync"
	"time"

	appdb "semantic-search/internal/database"
	"semantic-search/internal/embedder"
	"semantic-search/internal/storage"
)

const (
	DefaultImageWorkers  = 1
	DefaultVideoWorkers  = 1
	DefaultMaxImageBytes = 32 << 20 // 32 MiB
)

const maxAttempts = 3

// Config configures ingestion webhook auth, independent worker capacities, and
// object-size limits.
type Config struct {
	WebhookToken  string
	ImageWorkers  int
	VideoWorkers  int
	MaxImageBytes int64
}

type objectFetcher interface {
	Get(ctx context.Context, bucket, key, version string) (storage.Object, error)
}

type imageEmbedder interface {
	GetImageEmbeddingContext(ctx context.Context, filename, contentType string, image io.Reader) (embedder.Result, error)
}

type videoHandlerFunc func(ctx context.Context, job *appdb.IngestionJob) (status string, reason string, err error)

type Service struct {
	db            *appdb.Store
	store         objectFetcher
	embedder      imageEmbedder
	token         string
	imageWorkers  int
	videoWorkers  int
	maxImageBytes int64
	leaseDuration time.Duration
	videoHandler  videoHandlerFunc

	enqueue       func(context.Context, appdb.EnqueueIngestionJobInput) (bool, error)
	processNext   func(context.Context) (bool, error)
	claimByClass  func(context.Context, string) (*appdb.IngestionJob, error)
	heartbeatJob  func(context.Context, int64, string) error
	completeJob   func(context.Context, int64, string, string, string) error
	failJob       func(context.Context, int64, string, string, string) error
	retryJob      func(context.Context, int64, string, time.Time, string) error
	saveEmbedding func(context.Context, appdb.SaveEmbeddingInput) error
}

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
	return NewWithConfig(db, store, client, Config{WebhookToken: webhookToken})
}

func NewWithConfig(db *appdb.Store, store *storage.Store, client *embedder.Client, cfg Config) *Service {
	imageWorkers := cfg.ImageWorkers
	if imageWorkers <= 0 {
		imageWorkers = DefaultImageWorkers
	}
	videoWorkers := cfg.VideoWorkers
	if videoWorkers <= 0 {
		videoWorkers = DefaultVideoWorkers
	}
	maxImageBytes := cfg.MaxImageBytes
	if maxImageBytes <= 0 {
		maxImageBytes = DefaultMaxImageBytes
	}

	s := &Service{
		db:            db,
		store:         store,
		embedder:      client,
		token:         strings.TrimSpace(cfg.WebhookToken),
		imageWorkers:  imageWorkers,
		videoWorkers:  videoWorkers,
		maxImageBytes: maxImageBytes,
		leaseDuration: appdb.DefaultIngestionJobLeaseDuration,
	}
	if db != nil {
		s.enqueue = db.EnqueueIngestionJob
		s.claimByClass = db.ClaimIngestionJobByClass
		s.heartbeatJob = db.HeartbeatIngestionJob
		s.completeJob = db.CompleteIngestionJob
		s.failJob = db.FailIngestionJob
		s.retryJob = db.RetryIngestionJob
		s.saveEmbedding = db.SaveDocumentEmbedding
	}
	return s
}

// HandleEvent accepts S3-compatible ObjectCreated notifications. It classifies
// and records durable jobs; slow object fetching and embedding are performed by
// class-isolated workers in Run.
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
		mediaClass := ClassifyMedia(record.S3.Object.ContentType, key)
		inserted, err := s.enqueue(r.Context(), appdb.EnqueueIngestionJobInput{
			Bucket:        record.S3.Bucket.Name,
			ObjectKey:     key,
			ObjectVersion: record.S3.Object.VersionID,
			ETag:          record.S3.Object.ETag,
			SizeBytes:     &size,
			ContentType:   record.S3.Object.ContentType,
			MediaClass:    mediaClass,
		})
		if err != nil {
			log.Printf("S3 webhook object failed bucket=%q key=%q media_class=%q event=%q error=%q", record.S3.Bucket.Name, key, mediaClass, record.EventName, err)
			http.Error(w, "could not record ingestion job", http.StatusInternalServerError)
			return
		}
		accepted++
		if inserted {
			log.Printf("S3 webhook object inserted bucket=%q key=%q media_class=%q event=%q etag=%q", record.S3.Bucket.Name, key, mediaClass, record.EventName, record.S3.Object.ETag)
		} else {
			log.Printf("S3 webhook object duplicate bucket=%q key=%q media_class=%q event=%q etag=%q", record.S3.Bucket.Name, key, mediaClass, record.EventName, record.S3.Object.ETag)
		}
	}
	log.Printf("S3 webhook request completed remote=%q records=%d accepted=%d", r.RemoteAddr, len(notification.Records), accepted)
	w.WriteHeader(http.StatusNoContent)
}

func newEventService(token string, enqueue func(context.Context, appdb.EnqueueIngestionJobInput) (bool, error)) *Service {
	return &Service{token: token, enqueue: enqueue}
}

// Run starts independent image and video worker pools and polls the durable
// queue until ctx is canceled or a worker returns a fatal error.
func (s *Service) Run(ctx context.Context, interval time.Duration) error {
	if interval <= 0 {
		interval = time.Second
	}
	if s.processNext != nil {
		return s.runLoop(ctx, interval, s.processNext)
	}

	imageWorkers := s.imageWorkers
	if imageWorkers <= 0 {
		imageWorkers = DefaultImageWorkers
	}
	videoWorkers := s.videoWorkers
	if videoWorkers <= 0 {
		videoWorkers = DefaultVideoWorkers
	}

	workerCtx, cancel := context.WithCancel(ctx)
	defer cancel()

	var (
		wg       sync.WaitGroup
		errOnce  sync.Once
		firstErr error
	)
	startWorker := func(mediaClass string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			err := s.runLoop(workerCtx, interval, func(loopCtx context.Context) (bool, error) {
				return s.ProcessNextClass(loopCtx, mediaClass)
			})
			if err != nil {
				errOnce.Do(func() {
					firstErr = err
					cancel()
				})
			}
		}()
	}

	for range imageWorkers {
		startWorker(appdb.IngestionMediaClassImage)
	}
	for range videoWorkers {
		startWorker(appdb.IngestionMediaClassVideo)
	}

	wg.Wait()
	return firstErr
}

func (s *Service) runLoop(ctx context.Context, interval time.Duration, step func(context.Context) (bool, error)) error {
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		processed, err := step(ctx)
		if err != nil {
			if ctx.Err() != nil {
				return nil
			}
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

// ProcessNextClass claims and processes a single job belonging to mediaClass.
func (s *Service) ProcessNextClass(ctx context.Context, mediaClass string) (bool, error) {
	job, err := s.claimByClass(ctx, mediaClass)
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

	leaseDuration := s.leaseDuration
	if leaseDuration <= 0 {
		leaseDuration = appdb.DefaultIngestionJobLeaseDuration
	}
	heartbeatInterval := max(leaseDuration/3, 10*time.Millisecond)

	go func() {
		defer close(heartbeatDone)
		ticker := time.NewTicker(heartbeatInterval)
		defer ticker.Stop()
		for {
			select {
			case <-jobCtx.Done():
				return
			case <-ticker.C:
				if err := s.heartbeatJob(jobCtx, job.ID, job.LeaseToken); err != nil {
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
	case hbErr := <-heartbeatErr:
		return fmt.Errorf("heartbeat ingestion job %d: %w", job.ID, hbErr)
	default:
	}
	if err != nil {
		return s.fail(ctx, job, err)
	}
	return s.completeJob(ctx, job.ID, job.LeaseToken, status, reason)
}

func (s *Service) processClaimed(ctx context.Context, job *appdb.IngestionJob) (string, string, error) {
	mediaClass := job.MediaClass
	if mediaClass == "" {
		mediaClass = ClassifyMedia(job.ContentType, job.ObjectKey)
	}

	switch mediaClass {
	case appdb.IngestionMediaClassImage:
		return s.processImage(ctx, job)
	case appdb.IngestionMediaClassVideo:
		if s.videoHandler != nil {
			return s.videoHandler(ctx, job)
		}
		return appdb.IngestionJobIgnored, "video ingestion is not implemented yet", nil
	case appdb.IngestionMediaClassUnsupported:
		return appdb.IngestionJobIgnored, "unsupported media type", nil
	default:
		return "", "", PermanentMediaError(fmt.Errorf("unknown media class %q", mediaClass))
	}
}

func (s *Service) processImage(ctx context.Context, job *appdb.IngestionJob) (string, string, error) {
	maxBytes := s.maxImageBytes
	if maxBytes <= 0 {
		maxBytes = DefaultMaxImageBytes
	}
	if job.SizeBytes != nil {
		if *job.SizeBytes == 0 {
			return "", "", PermanentMediaError(fmt.Errorf("image object %q is empty", job.ObjectKey))
		}
		if *job.SizeBytes > maxBytes {
			return "", "", PermanentLimitError(fmt.Errorf("image object %q size %d exceeds limit of %d bytes", job.ObjectKey, *job.SizeBytes, maxBytes))
		}
	}

	object, err := s.store.Get(ctx, job.Bucket, job.ObjectKey, job.ObjectVersion)
	if err != nil {
		return "", "", err
	}
	defer object.Body.Close()

	payload, err := io.ReadAll(io.LimitReader(object.Body, maxBytes+1))
	if err != nil {
		return "", "", RetryableError(fmt.Errorf("read object payload: %w", err))
	}
	if int64(len(payload)) > maxBytes {
		return "", "", PermanentLimitError(fmt.Errorf("image object %q exceeds limit of %d bytes", job.ObjectKey, maxBytes))
	}

	verifiedContentType, err := VerifyImageMedia(object.ContentType, job.ObjectKey, payload)
	if err != nil {
		return "", "", err
	}

	embedding, err := s.embedder.GetImageEmbeddingContext(ctx, path.Base(job.ObjectKey), verifiedContentType, bytes.NewReader(payload))
	if err != nil {
		return "", "", fmt.Errorf("embed object: %w", err)
	}

	sourceURI := (&url.URL{Scheme: "s3", Host: job.Bucket, Path: "/" + job.ObjectKey}).String()
	if err := s.saveEmbedding(ctx, appdb.SaveEmbeddingInput{
		SourceURI:   sourceURI,
		MediaType:   appdb.MediaTypeImage,
		ContentType: verifiedContentType,
		Model:       embedding.Model,
		DocumentMetadata: appdb.Metadata{
			"bucket":         job.Bucket,
			"object_key":     job.ObjectKey,
			"object_version": job.ObjectVersion,
			"etag":           job.ETag,
			"size_bytes":     int64(len(payload)),
		},
		EmbeddingMetadata: appdb.Metadata{"dimensions": len(embedding.Values)},
		Values:            embedding.Values,
	}); err != nil {
		return "", "", fmt.Errorf("save embedding: %w", err)
	}
	return appdb.IngestionJobCompleted, "", nil
}

func (s *Service) fail(ctx context.Context, job *appdb.IngestionJob, err error) error {
	failureKind, retryable := ClassifyError(err)
	if !retryable || job.Attempts >= maxAttempts {
		log.Printf("ingestion job failed permanently id=%d key=%q attempts=%d failure_kind=%q error=%q", job.ID, job.ObjectKey, job.Attempts, failureKind, err.Error())
		return s.failJob(ctx, job.ID, job.LeaseToken, failureKind, err.Error())
	}
	backoff := time.Duration(job.Attempts) * time.Second
	log.Printf("ingestion job scheduled for retry id=%d key=%q attempts=%d backoff=%s failure_kind=%q error=%q", job.ID, job.ObjectKey, job.Attempts, backoff, failureKind, err.Error())
	return s.retryJob(ctx, job.ID, job.LeaseToken, time.Now().Add(backoff), err.Error())
}
