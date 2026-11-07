//go:build integration

package indexstore

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"net"
	"net/url"
	"os"
	"slices"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
	"github.com/aws/smithy-go"
)

// These tests need SeaweedFS with S3 authentication enabled. See
// docs/local-dev.md for the docker run command that matches the defaults
// below. They fail, never skip, when the emulator is unreachable.
const (
	defaultEmulatorEndpoint  = "http://localhost:8333"
	defaultEmulatorAccessKey = "indexstore-test-access"
	defaultEmulatorSecretKey = "indexstore-test-secret"
)

func envOr(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}

// emulator prepares the SDK environment and returns the endpoint, failing the
// test if nothing is listening there.
func emulator(t *testing.T) string {
	t.Helper()
	endpoint := envOr("INDEXSTORE_S3_ENDPOINT", defaultEmulatorEndpoint)
	setAWSEnv(
		t,
		envOr("INDEXSTORE_S3_ACCESS_KEY", defaultEmulatorAccessKey),
		envOr("INDEXSTORE_S3_SECRET_KEY", defaultEmulatorSecretKey),
	)
	u, err := url.Parse(endpoint)
	if err != nil {
		t.Fatalf("bad INDEXSTORE_S3_ENDPOINT %q: %v", endpoint, err)
	}
	conn, err := net.DialTimeout("tcp", u.Host, 3*time.Second)
	if err != nil {
		t.Fatalf(
			"S3 emulator unreachable at %s: %v (start SeaweedFS as described in docs/local-dev.md)",
			endpoint,
			err,
		)
	}
	_ = conn.Close()
	return endpoint
}

var bucketSeq = time.Now().UnixNano()

// newBucket creates a bucket unique to this test run.
func newBucket(t *testing.T, client *s3.Client) string {
	t.Helper()
	bucketSeq++
	bucket := fmt.Sprintf("indexstore-%d", bucketSeq)
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	if _, err := client.CreateBucket(
		ctx,
		&s3.CreateBucketInput{Bucket: aws.String(bucket)},
	); err != nil {
		t.Fatalf("create bucket %s: %v", bucket, err)
	}
	return bucket
}

func TestS3ContractSeaweedFS(t *testing.T) {
	endpoint := emulator(t)
	raw := rawS3Client(t, endpoint)
	bucket := newBucket(t, raw)
	n := 0
	runContract(t, func(t *testing.T) backend {
		n++
		key := fmt.Sprintf("case-%d/recipe-index.json.gz", n)
		return backend{
			open: func(version int) Store {
				return newTestStore(t, Config{
					Mode:          ModeS3,
					S3Bucket:      bucket,
					S3Key:         key,
					S3Endpoint:    endpoint,
					FormatVersion: version,
					S3MaxAttempts: 1,
				})
			},
			put: func(data []byte) {
				_, err := raw.PutObject(context.Background(), &s3.PutObjectInput{
					Bucket: aws.String(bucket),
					Key:    aws.String(key),
					Body:   bytes.NewReader(data),
				})
				if err != nil {
					t.Fatalf("plant object: %v", err)
				}
			},
		}
	})
}

func TestS3StoredObjectIsGzip(t *testing.T) {
	endpoint := emulator(t)
	raw := rawS3Client(t, endpoint)
	bucket := newBucket(t, raw)
	s := newTestStore(
		t,
		Config{Mode: ModeS3, S3Bucket: bucket, S3Endpoint: endpoint, S3MaxAttempts: 1},
	)
	if err := s.Save(context.Background(), Snapshot{Payload: payloadOf(t, 100)}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	out, err := raw.GetObject(context.Background(), &s3.GetObjectInput{
		Bucket: aws.String(bucket),
		Key:    aws.String(DefaultS3Key),
	})
	if err != nil {
		t.Fatalf("get object at default key: %v", err)
	}
	defer out.Body.Close()
	magic := make([]byte, 2)
	if _, err := out.Body.Read(magic); err != nil || magic[0] != 0x1f || magic[1] != 0x8b {
		t.Errorf("object does not start with the gzip magic bytes: %x (%v)", magic, err)
	}
}

func TestS3ProtocolErrors(t *testing.T) {
	endpoint := emulator(t)
	raw := rawS3Client(t, endpoint)
	bucket := newBucket(t, raw)
	snap := Snapshot{Payload: []byte(`[]`)}

	tests := []struct {
		name     string
		keys     [2]string
		bucket   string
		wantCode []string
		// skipSave is set where the emulator deviates from AWS: SeaweedFS creates a
		// missing bucket on PUT instead of returning NoSuchBucket.
		skipSave bool
	}{
		{
			"missing bucket",
			[2]string{defaultEmulatorAccessKey, defaultEmulatorSecretKey},
			fmt.Sprintf("indexstore-missing-%d", time.Now().UnixNano()),
			[]string{"NoSuchBucket"},
			true,
		},
		{
			"wrong secret",
			[2]string{defaultEmulatorAccessKey, "wrong-secret"},
			bucket,
			[]string{"SignatureDoesNotMatch", "AccessDenied"},
			false,
		},
		{
			"unknown access key",
			[2]string{"unknown-access", "unknown-secret"},
			bucket,
			[]string{"InvalidAccessKeyId", "AccessDenied", "SignatureDoesNotMatch"},
			false,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setAWSEnv(t, tt.keys[0], tt.keys[1])
			s := newTestStore(t, Config{
				Mode:          ModeS3,
				S3Bucket:      tt.bucket,
				S3Endpoint:    endpoint,
				S3MaxAttempts: 1,
			})
			for op, run := range map[string]func() (bool, error){
				"Load": func() (
					bool,
					error,
				) {
					_, found, err := s.Load(context.Background())
					return found, err
				},
				"Save": func() (bool, error) { return false, s.Save(context.Background(), snap) },
			} {
				if op == "Save" && tt.skipSave {
					continue
				}
				found, err := run()
				if err == nil || found {
					t.Fatalf("%s = found %v, err %v; want an error, not absent", op, found, err)
				}
				var apiErr smithy.APIError
				if !errors.As(err, &apiErr) {
					t.Fatalf("%s error %v carries no S3 error code", op, err)
				}
				if !slices.Contains(tt.wantCode, apiErr.ErrorCode()) {
					t.Errorf(
						"%s error code = %s, want one of %v",
						op,
						apiErr.ErrorCode(),
						tt.wantCode,
					)
				}
			}
		})
	}
}
