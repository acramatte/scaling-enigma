package storage

import (
	"context"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/credentials"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

// Object describes an opened S3-compatible object. Call Close when finished.
type Object struct {
	Body        io.ReadCloser
	ContentType string
}

type Store struct {
	client *s3.Client
}

// NewFromEnvironment creates an S3-compatible client. Set S3_ENDPOINT for
// local storage such as RustFS; leave it unset for normal AWS endpoint resolution.
func NewFromEnvironment(ctx context.Context) (*Store, error) {
	region := strings.TrimSpace(os.Getenv("S3_REGION"))
	if region == "" {
		region = "us-east-1"
	}

	options := []func(*awsconfig.LoadOptions) error{awsconfig.WithRegion(region)}
	endpoint := strings.TrimRight(strings.TrimSpace(os.Getenv("S3_ENDPOINT")), "/")
	if endpoint != "" {
		// S3-compatible stores are not required to implement AWS's optional
		// response-checksum extension. Request validation only when an operation
		// explicitly requires it; otherwise the SDK warns for every valid RustFS
		// GetObject response that has no x-amz-checksum-* header.
		options = append(options, awsconfig.WithResponseChecksumValidation(aws.ResponseChecksumValidationWhenRequired))
	}
	accessKey := strings.TrimSpace(os.Getenv("S3_ACCESS_KEY"))
	secretKey := strings.TrimSpace(os.Getenv("S3_SECRET_KEY"))
	if accessKey != "" || secretKey != "" {
		if accessKey == "" || secretKey == "" {
			return nil, fmt.Errorf("both S3_ACCESS_KEY and S3_SECRET_KEY must be set")
		}
		options = append(options, awsconfig.WithCredentialsProvider(credentials.NewStaticCredentialsProvider(accessKey, secretKey, "")))
	}

	config, err := awsconfig.LoadDefaultConfig(ctx, options...)
	if err != nil {
		return nil, fmt.Errorf("load S3 configuration: %w", err)
	}
	client := s3.NewFromConfig(config, func(options *s3.Options) {
		if endpoint != "" {
			options.BaseEndpoint = aws.String(endpoint)
			options.UsePathStyle = true
		}
	})
	return &Store{client: client}, nil
}

func (s *Store) Get(ctx context.Context, bucket, key, version string) (Object, error) {
	input := &s3.GetObjectInput{Bucket: aws.String(bucket), Key: aws.String(key)}
	if version != "" {
		input.VersionId = aws.String(version)
	}
	result, err := s.client.GetObject(ctx, input)
	if err != nil {
		return Object{}, fmt.Errorf("get s3://%s/%s: %w", bucket, key, err)
	}
	return Object{Body: result.Body, ContentType: aws.ToString(result.ContentType)}, nil
}
