package indexstore

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/aws/aws-sdk-go-v2/aws"
	"github.com/aws/aws-sdk-go-v2/service/s3"
)

func newTestStore(t *testing.T, cfg Config) Store {
	t.Helper()
	s, err := New(context.Background(), cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return s
}

func TestFilesystemContract(t *testing.T) {
	runContract(t, func(t *testing.T) backend {
		path := filepath.Join(t.TempDir(), "snap", "recipe-index.json.gz")
		return backend{
			open: func(version int) Store {
				return newTestStore(t, Config{Mode: ModeFilesystem, FilePath: path, FormatVersion: version})
			},
			put: func(raw []byte) {
				if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
					t.Fatalf("mkdir: %v", err)
				}
				if err := os.WriteFile(path, raw, 0o600); err != nil {
					t.Fatalf("plant file: %v", err)
				}
			},
		}
	})
}

func TestS3ContractFake(t *testing.T) {
	runContract(t, func(t *testing.T) backend {
		setAWSEnv(t, fakeAccessKey, fakeSecretKey)
		fake, srv := newFakeS3(t)
		_ = fake
		const bucket, key = "bucket", "paprika-mcp-go/recipe-index.json.gz"
		raw := rawS3Client(t, srv.URL)
		return backend{
			open: func(version int) Store {
				return newTestStore(t, Config{
					Mode: ModeS3, S3Bucket: bucket, S3Key: key, S3Endpoint: srv.URL,
					FormatVersion: version, S3MaxAttempts: 1,
				})
			},
			put: func(data []byte) {
				_, err := raw.PutObject(context.Background(), &s3.PutObjectInput{
					Bucket: aws.String(bucket), Key: aws.String(key), Body: bytes.NewReader(data),
				})
				if err != nil {
					t.Fatalf("plant object: %v", err)
				}
			},
		}
	})
}

func TestFilesystemSaveDetails(t *testing.T) {
	path := filepath.Join(t.TempDir(), "idx.gz")
	s := newTestStore(t, Config{Mode: ModeFilesystem, FilePath: path})
	for range 3 {
		if err := s.Save(context.Background(), Snapshot{Payload: []byte(`{}`)}); err != nil {
			t.Fatalf("Save: %v", err)
		}
	}
	entries, err := os.ReadDir(filepath.Dir(path))
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 || entries[0].Name() != "idx.gz" {
		t.Errorf("directory holds %v, want only idx.gz (no temporary files)", entries)
	}
	if runtime.GOOS != "windows" {
		info, err := os.Stat(path)
		if err != nil {
			t.Fatal(err)
		}
		if mode := info.Mode().Perm(); mode != 0o600 {
			t.Errorf("file mode = %o, want 600", mode)
		}
	}
}

func TestSaveRejectsEmptyPayload(t *testing.T) {
	s := newTestStore(t, Config{Mode: ModeFilesystem, FilePath: filepath.Join(t.TempDir(), "x")})
	if err := s.Save(context.Background(), Snapshot{}); err == nil {
		t.Fatal("Save with empty payload succeeded, want error")
	}
}

func TestNone(t *testing.T) {
	s := newTestStore(t, Config{})
	if err := s.Save(context.Background(), Snapshot{Payload: []byte(`{}`)}); err != nil {
		t.Fatalf("Save: %v", err)
	}
	if _, found, err := s.Load(context.Background()); found || err != nil {
		t.Fatalf("Load = found %v, err %v; want absent", found, err)
	}
}

func TestConfigFromEnv(t *testing.T) {
	tests := []struct {
		name    string
		env     map[string]string
		want    Config
		wantErr string
	}{
		{"default is none", map[string]string{}, Config{Mode: ModeNone, S3Key: DefaultS3Key}, ""},
		{"filesystem", map[string]string{"INDEX_STORE": "filesystem", "INDEX_FILE_PATH": "/data/i.gz"},
			Config{Mode: ModeFilesystem, FilePath: "/data/i.gz", S3Key: DefaultS3Key}, ""},
		{"filesystem needs path", map[string]string{"INDEX_STORE": "filesystem"}, Config{}, "INDEX_FILE_PATH"},
		{"s3 defaults key", map[string]string{"INDEX_STORE": "s3", "INDEX_S3_BUCKET": "b"},
			Config{Mode: ModeS3, S3Bucket: "b", S3Key: DefaultS3Key}, ""},
		{"s3 full", map[string]string{
			"INDEX_STORE": " S3 ", "INDEX_S3_BUCKET": "b", "INDEX_S3_KEY": "k", "INDEX_S3_ENDPOINT": "http://x:1",
		}, Config{Mode: ModeS3, S3Bucket: "b", S3Key: "k", S3Endpoint: "http://x:1"}, ""},
		{"s3 needs bucket", map[string]string{"INDEX_STORE": "s3"}, Config{}, "INDEX_S3_BUCKET"},
		{"unknown mode", map[string]string{"INDEX_STORE": "redis"}, Config{}, "unknown INDEX_STORE"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			got, err := ConfigFromEnv(func(k string) string { return tt.env[k] })
			if tt.wantErr != "" {
				if err == nil || !strings.Contains(err.Error(), tt.wantErr) {
					t.Fatalf("err = %v, want containing %q", err, tt.wantErr)
				}
				return
			}
			if err != nil {
				t.Fatalf("unexpected error: %v", err)
			}
			if got != tt.want {
				t.Errorf("got %+v, want %+v", got, tt.want)
			}
		})
	}
}

func TestNewRejectsBadConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"unknown mode", Config{Mode: "tape"}},
		{"filesystem without path", Config{Mode: ModeFilesystem}},
		{"s3 without bucket", Config{Mode: ModeS3}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(context.Background(), tt.cfg); err == nil {
				t.Fatal("New succeeded, want error")
			}
		})
	}
}

func TestS3NoRegionWithoutEndpointFails(t *testing.T) {
	setAWSEnv(t, fakeAccessKey, fakeSecretKey)
	t.Setenv("AWS_REGION", "")
	if _, err := New(context.Background(), Config{Mode: ModeS3, S3Bucket: "b"}); err == nil {
		t.Fatal("New succeeded without a region, want error")
	}
}

func TestS3SaveErrors(t *testing.T) {
	tests := []struct {
		name   string
		setup  func(f *fakeS3)
		cfg    Config
		wantIn string
	}{
		{"500 on PUT", func(f *fakeS3) { f.failPuts.Store(100) }, Config{S3MaxAttempts: 1}, "InternalError"},
		{"timeout", func(f *fakeS3) { f.hang.Store(true) },
			Config{S3MaxAttempts: 1, OperationTimeout: 150 * time.Millisecond}, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			setAWSEnv(t, fakeAccessKey, fakeSecretKey)
			fake, srv := newFakeS3(t)
			tt.setup(fake)
			cfg := tt.cfg
			cfg.Mode, cfg.S3Bucket, cfg.S3Endpoint = ModeS3, "bucket", srv.URL
			s := newTestStore(t, cfg)
			err := s.Save(context.Background(), Snapshot{Payload: []byte(`[]`)})
			if err == nil {
				t.Fatal("Save succeeded, want error")
			}
			if !strings.Contains(err.Error(), tt.wantIn) {
				t.Errorf("err = %v, want containing %q", err, tt.wantIn)
			}
			if _, _, err := s.Load(context.Background()); tt.name == "timeout" && err == nil {
				t.Error("Load succeeded against a hung server, want error")
			}
		})
	}
}

func TestSaverRetriesNextIntervalAfterS3Failure(t *testing.T) {
	setAWSEnv(t, fakeAccessKey, fakeSecretKey)
	fake, srv := newFakeS3(t)
	fake.failPuts.Store(1)
	s := newTestStore(t, Config{Mode: ModeS3, S3Bucket: "bucket", S3Endpoint: srv.URL, S3MaxAttempts: 1})

	saver := NewSaver(s, func(context.Context) (Snapshot, error) {
		return Snapshot{Payload: []byte(`["x"]`)}, nil
	}, SaverConfig{Interval: 30 * time.Millisecond, Logger: slog.New(slog.DiscardHandler)})
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { saver.Run(ctx); close(done) }()
	saver.MarkDirty()

	waitFor(t, func() bool { _, ok := fake.object("bucket", DefaultS3Key); return ok })
	if got := fake.puts.Load(); got != 2 {
		t.Errorf("PUT attempts = %d, want 2 (one failure, one retry)", got)
	}
	cancel()
	<-done
}

type memStore struct {
	saves   atomic.Int32
	failFor atomic.Int32
}

func (m *memStore) Load(context.Context) (Snapshot, bool, error) { return Snapshot{}, false, nil }

func (m *memStore) Save(context.Context, Snapshot) error {
	m.saves.Add(1)
	if m.failFor.Add(-1) >= 0 {
		return errors.New("injected failure")
	}
	return nil
}

func TestSaver(t *testing.T) {
	source := func(context.Context) (Snapshot, error) { return Snapshot{Payload: []byte(`[]`)}, nil }
	quiet := slog.New(slog.DiscardHandler)

	t.Run("no save without a change", func(t *testing.T) {
		m := &memStore{}
		saver := NewSaver(m, source, SaverConfig{Interval: 10 * time.Millisecond, Logger: quiet})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { saver.Run(ctx); close(done) }()
		time.Sleep(80 * time.Millisecond)
		cancel()
		<-done
		if got := m.saves.Load(); got != 0 {
			t.Errorf("saves = %d, want 0", got)
		}
	})

	t.Run("one save per change batch", func(t *testing.T) {
		m := &memStore{}
		saver := NewSaver(m, source, SaverConfig{Interval: 20 * time.Millisecond, Logger: quiet})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { saver.Run(ctx); close(done) }()
		for range 50 {
			saver.MarkDirty()
		}
		waitFor(t, func() bool { return m.saves.Load() >= 1 })
		time.Sleep(100 * time.Millisecond)
		cancel()
		<-done
		if got := m.saves.Load(); got != 1 {
			t.Errorf("saves = %d, want 1", got)
		}
	})

	t.Run("failed saves retry", func(t *testing.T) {
		m := &memStore{}
		m.failFor.Store(2)
		saver := NewSaver(m, source, SaverConfig{Interval: 15 * time.Millisecond, Logger: quiet})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { saver.Run(ctx); close(done) }()
		saver.MarkDirty()
		waitFor(t, func() bool { return m.saves.Load() >= 3 })
		cancel()
		<-done
		if got := m.saves.Load(); got != 3 {
			t.Errorf("saves = %d, want 3 (two failures then success)", got)
		}
	})

	t.Run("final attempt on shutdown", func(t *testing.T) {
		m := &memStore{}
		saver := NewSaver(m, source, SaverConfig{Interval: time.Hour, Logger: quiet})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { saver.Run(ctx); close(done) }()
		saver.MarkDirty()
		cancel()
		<-done
		if got := m.saves.Load(); got != 1 {
			t.Errorf("saves = %d, want 1 final attempt", got)
		}
	})

	t.Run("source error is retried", func(t *testing.T) {
		m := &memStore{}
		var calls atomic.Int32
		src := func(context.Context) (Snapshot, error) {
			if calls.Add(1) == 1 {
				return Snapshot{}, errors.New("not ready")
			}
			return Snapshot{Payload: []byte(`[]`)}, nil
		}
		saver := NewSaver(m, src, SaverConfig{Interval: 15 * time.Millisecond, Logger: quiet})
		ctx, cancel := context.WithCancel(context.Background())
		done := make(chan struct{})
		go func() { saver.Run(ctx); close(done) }()
		saver.MarkDirty()
		waitFor(t, func() bool { return m.saves.Load() >= 1 })
		cancel()
		<-done
	})
}

func waitFor(t *testing.T, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatal("condition not met within 5s")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestSaverStaysSilentForTheNoneStore(t *testing.T) {
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	called := false
	source := func(context.Context) (Snapshot, error) {
		called = true
		return Snapshot{Payload: []byte(`[]`)}, nil
	}
	saver := NewSaver(newTestStore(t, Config{}), source, SaverConfig{Interval: 10 * time.Millisecond, Logger: logger})
	saver.MarkDirty()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { saver.Run(ctx); close(done) }()
	time.Sleep(60 * time.Millisecond)
	cancel()
	<-done
	if called || logs.Len() != 0 {
		t.Errorf("source called = %v, log = %q; want neither for the none store", called, logs.String())
	}
}
