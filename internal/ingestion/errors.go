package ingestion

import (
	"errors"
	"net/http"

	appdb "semantic-search/internal/database"
	"semantic-search/internal/embedder"

	awshttp "github.com/aws/aws-sdk-go-v2/aws/transport/http"
	"github.com/aws/smithy-go"
)

// ProcessingError tags an ingestion failure with whether the worker should
// retry it or mark the job permanently failed immediately.
type ProcessingError struct {
	Kind string
	Err  error
}

func (e *ProcessingError) Error() string {
	return e.Err.Error()
}

func (e *ProcessingError) Unwrap() error {
	return e.Err
}

func PermanentMediaError(err error) error {
	if err == nil {
		return nil
	}
	return &ProcessingError{Kind: appdb.IngestionFailurePermanentMedia, Err: err}
}

func PermanentLimitError(err error) error {
	if err == nil {
		return nil
	}
	return &ProcessingError{Kind: appdb.IngestionFailurePermanentLimit, Err: err}
}

func RetryableError(err error) error {
	if err == nil {
		return nil
	}
	return &ProcessingError{Kind: appdb.IngestionFailureRetryable, Err: err}
}

// ClassifyError separates retryable transport/inference failures from
// permanent media and operating-limit failures.
func ClassifyError(err error) (failureKind string, retryable bool) {
	if err == nil {
		return appdb.IngestionFailureNone, false
	}

	var procErr *ProcessingError
	if errors.As(err, &procErr) {
		return procErr.Kind, procErr.Kind == appdb.IngestionFailureRetryable
	}

	var statusErr *embedder.StatusError
	if errors.As(err, &statusErr) {
		switch statusErr.StatusCode {
		case http.StatusRequestEntityTooLarge:
			return appdb.IngestionFailurePermanentLimit, false
		case http.StatusBadRequest, http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
			return appdb.IngestionFailurePermanentMedia, false
		default:
			return appdb.IngestionFailureRetryable, true
		}
	}

	var apiErr smithy.APIError
	if errors.As(err, &apiErr) {
		switch apiErr.ErrorCode() {
		case "NoSuchKey", "NoSuchVersion", "NoSuchBucket", "InvalidArgument", "PreconditionFailed":
			return appdb.IngestionFailurePermanentMedia, false
		}
	}

	var respErr *awshttp.ResponseError
	if errors.As(err, &respErr) {
		switch respErr.HTTPStatusCode() {
		case http.StatusRequestEntityTooLarge:
			return appdb.IngestionFailurePermanentLimit, false
		case http.StatusBadRequest, http.StatusNotFound, http.StatusPreconditionFailed,
			http.StatusUnsupportedMediaType, http.StatusUnprocessableEntity:
			return appdb.IngestionFailurePermanentMedia, false
		default:
			return appdb.IngestionFailureRetryable, true
		}
	}

	return appdb.IngestionFailureRetryable, true
}
