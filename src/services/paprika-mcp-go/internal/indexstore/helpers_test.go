package indexstore

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/aws/aws-sdk-go-v2/aws"
	awsconfig "github.com/aws/aws-sdk-go-v2/config"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

const (
	fakeAccessKey = "fake-access"
	fakeSecretKey = "fake-secret"
)

// setAWSEnv points the SDK's default credential chain at fixed test keys and
// isolates it from the developer's own AWS files and instance metadata.
func setAWSEnv(t *testing.T, accessKey, secretKey string) {
	t.Helper()
	t.Setenv("AWS_ACCESS_KEY_ID", accessKey)
	t.Setenv("AWS_SECRET_ACCESS_KEY", secretKey)
	t.Setenv("AWS_SESSION_TOKEN", "")
	t.Setenv("AWS_PROFILE", "")
	t.Setenv("AWS_REGION", "us-east-1")
	t.Setenv("AWS_EC2_METADATA_DISABLED", "true")
	t.Setenv("AWS_SHARED_CREDENTIALS_FILE", filepath.Join(t.TempDir(), "credentials"))
	t.Setenv("AWS_CONFIG_FILE", filepath.Join(t.TempDir(), "config"))
}

// rawS3Client returns an SDK client that bypasses indexstore, so tests can
// plant and inspect objects directly.
func rawS3Client(t *testing.T, endpoint string) *s3.Client {
	t.Helper()
	awsCfg, err := awsconfig.LoadDefaultConfig(context.Background())
	if err != nil {
		t.Fatalf("load AWS config: %v", err)
	}
	return s3.NewFromConfig(awsCfg, func(o *s3.Options) {
		o.BaseEndpoint = aws.String(endpoint)
		o.UsePathStyle = true
	})
}

// payloadOf builds a compact JSON array of n synthetic entries.
func payloadOf(t *testing.T, n int) json.RawMessage {
	t.Helper()
	entries := make([]map[string]any, n)
	for i := range entries {
		entries[i] = map[string]any{
			"uid":    fmt.Sprintf("uid-%05d", i),
			"hash":   fmt.Sprintf("hash-%05d", i),
			"name":   fmt.Sprintf("Synthetic recipe %d <&>", i),
			"tokens": []string{"flour", "sugar", fmt.Sprintf("t%d", i%97)},
		}
	}
	b, err := json.Marshal(entries)
	if err != nil {
		t.Fatalf("marshal payload: %v", err)
	}
	return b
}

// fakeS3 is a minimal in-memory, path-style S3 endpoint. It does not verify
// signatures; failure knobs let tests inject server errors and hangs.
type fakeS3 struct {
	mu      sync.Mutex
	objects map[string][]byte
	puts    atomic.Int32
	// failPuts is how many upcoming PUT requests answer 500.
	failPuts atomic.Int32
	// hang makes every request block until the client gives up.
	hang atomic.Bool
	// release unblocks hung handlers at test cleanup.
	release chan struct{}
}

func newFakeS3(t *testing.T) (*fakeS3, *httptest.Server) {
	t.Helper()
	f := &fakeS3{objects: map[string][]byte{}, release: make(chan struct{})}
	srv := httptest.NewServer(f)
	t.Cleanup(srv.Close)
	t.Cleanup(func() { close(f.release) })
	return f, srv
}

func (f *fakeS3) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	if f.hang.Load() {
		select {
		case <-r.Context().Done():
		case <-f.release:
		}
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/")
	switch r.Method {
	case http.MethodPut:
		f.puts.Add(1)
		if f.failPuts.Add(-1) >= 0 {
			s3Error(w, http.StatusInternalServerError, "InternalError")
			return
		}
		body, err := readBody(r)
		if err != nil {
			s3Error(w, http.StatusBadRequest, "IncompleteBody")
			return
		}
		f.mu.Lock()
		f.objects[path] = body
		f.mu.Unlock()
		w.WriteHeader(http.StatusOK)
	case http.MethodGet:
		f.mu.Lock()
		body, ok := f.objects[path]
		f.mu.Unlock()
		if !ok {
			s3Error(w, http.StatusNotFound, "NoSuchKey")
			return
		}
		_, _ = w.Write(body)
	default:
		s3Error(w, http.StatusMethodNotAllowed, "MethodNotAllowed")
	}
}

func (f *fakeS3) object(bucket, key string) ([]byte, bool) {
	f.mu.Lock()
	defer f.mu.Unlock()
	b, ok := f.objects[bucket+"/"+key]
	return b, ok
}

func s3Error(w http.ResponseWriter, status int, code string) {
	w.Header().Set("Content-Type", "application/xml")
	w.WriteHeader(status)
	_, _ = fmt.Fprintf(w, `<?xml version="1.0" encoding="UTF-8"?><Error><Code>%s</Code><Message>%s</Message></Error>`, code, code)
}

// readBody returns the request body, decoding aws-chunked framing if the SDK
// used it for a streaming checksum.
func readBody(r *http.Request) ([]byte, error) {
	if !strings.Contains(r.Header.Get("Content-Encoding"), "aws-chunked") &&
		!strings.HasPrefix(r.Header.Get("X-Amz-Content-Sha256"), "STREAMING-") {
		return io.ReadAll(r.Body)
	}
	br := bufio.NewReader(r.Body)
	var out bytes.Buffer
	for {
		line, err := br.ReadString('\n')
		if err != nil {
			return nil, err
		}
		sizeText, _, _ := strings.Cut(strings.TrimSpace(line), ";")
		size, err := strconv.ParseInt(sizeText, 16, 64)
		if err != nil {
			return nil, err
		}
		if size == 0 {
			return out.Bytes(), nil
		}
		if _, err := io.CopyN(&out, br, size); err != nil {
			return nil, err
		}
		if _, err := br.Discard(2); err != nil {
			return nil, err
		}
	}
}
