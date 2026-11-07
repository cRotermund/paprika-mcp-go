package indexstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/aws-sdk-go-v2/service/s3/types"
	"github.com/aws/smithy-go"
	"github.com/aws/smithy-go/logging"
)

// emulatorRegion is used when an endpoint override is set and the SDK settings
// name no region, because emulators do not care which region signs a request.
const emulatorRegion = "us-east-1"

type s3Store struct {
	client  *s3.Client
	bucket  string
	key     string
	version int
	timeout time.Duration
}

func newS3Store(ctx context.Context, cfg Config) (*s3Store, error) {
	awsCfg, err := awsconfig.LoadDefaultConfig(ctx)
	if err != nil {
		return nil, fmt.Errorf("indexstore: load AWS configuration: %w", err)
	}
	awsCfg.Logger = logging.Nop{}
	if cfg.S3Endpoint != "" && awsCfg.Region == "" {
		awsCfg.Region = emulatorRegion
	}
	if awsCfg.Region == "" {
		return nil, errors.New("indexstore: no AWS region configured (set AWS_REGION)")
	}
	client := s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		if cfg.S3Endpoint != "" {
			o.BaseEndpoint = aws.String(cfg.S3Endpoint)
			o.UsePathStyle = true
		}
		if cfg.S3MaxAttempts > 0 {
			o.RetryMaxAttempts = cfg.S3MaxAttempts
		}
	})
	key := cfg.S3Key
	if key == "" {
		key = DefaultS3Key
	}
	timeout := cfg.OperationTimeout
	if timeout <= 0 {
		timeout = defaultOperationTimeout
	}
	return &s3Store{
		client:  client,
		bucket:  cfg.S3Bucket,
		key:     key,
		version: cfg.FormatVersion,
		timeout: timeout,
	}, nil
}

func (s *s3Store) Load(ctx context.Context) (Snapshot, bool, error) {
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	out, err := s.client.GetObject(ctx, &s3.GetObjectInput{Bucket: &s.bucket, Key: &s.key})
	if err != nil {
		if isNoSuchKey(err) {
			return Snapshot{}, false, nil
		}
		return Snapshot{}, false, fmt.Errorf("indexstore: get snapshot object: %w", err)
	}
	defer out.Body.Close()
	data, err := io.ReadAll(io.LimitReader(out.Body, maxDecodedSize))
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("indexstore: read snapshot object: %w", err)
	}
	snap, err := decode(data, s.version)
	if err != nil {
		return Snapshot{}, false, err
	}
	return snap, true, nil
}

func (s *s3Store) Save(ctx context.Context, snap Snapshot) error {
	data, err := encode(snap, s.version)
	if err != nil {
		return err
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	_, err = s.client.PutObject(ctx, &s3.PutObjectInput{
		Bucket:      &s.bucket,
		Key:         &s.key,
		Body:        bytes.NewReader(data),
		ContentType: aws.String("application/gzip"),
	})
	if err != nil {
		return fmt.Errorf("indexstore: put snapshot object: %w", err)
	}
	return nil
}

func isNoSuchKey(err error) bool {
	var nsk *types.NoSuchKey
	if errors.As(err, &nsk) {
		return true
	}
	var apiErr smithy.APIError
	return errors.As(err, &apiErr) && apiErr.ErrorCode() == "NoSuchKey"
}
