package indexstore

import (
	"context"
	"fmt"
	"strings"
	"time"
)

// Mode selects where the snapshot lives.
type Mode string

// Snapshot modes, matching INDEX_STORE.
const (
	ModeNone       Mode = "none"
	ModeFilesystem Mode = "filesystem"
	ModeS3         Mode = "s3"
)

// DefaultS3Key is the object key used when INDEX_S3_KEY is unset.
const DefaultS3Key = "paprika-mcp-go/recipe-index.json.gz"

const defaultOperationTimeout = 30 * time.Second

// Config describes a snapshot store.
type Config struct {
	Mode Mode

	// FormatVersion is the caller's current snapshot schema version. Load
	// rejects snapshots written with any other version. Zero means DefaultFormatVersion.
	FormatVersion int

	// FilePath is the snapshot file, for ModeFilesystem.
	FilePath string

	// S3Bucket and S3Key locate the object, for ModeS3. S3Endpoint overrides
	// the AWS endpoint and switches to path-style addressing.
	S3Bucket   string
	S3Key      string
	S3Endpoint string

	// OperationTimeout bounds each S3 request. Zero means 30 seconds.
	OperationTimeout time.Duration
	// S3MaxAttempts caps SDK attempts per request. Zero keeps the SDK default.
	S3MaxAttempts int
}

// ConfigFromEnv reads INDEX_STORE, INDEX_FILE_PATH, INDEX_S3_BUCKET,
// INDEX_S3_KEY, and INDEX_S3_ENDPOINT. An unknown mode or a mode missing its
// required settings is an error, so startup fails rather than silently
// skipping snapshots. The caller sets FormatVersion on the result.
func ConfigFromEnv(getenv func(string) string) (Config, error) {
	cfg := Config{
		Mode:       Mode(strings.ToLower(strings.TrimSpace(getenv("INDEX_STORE")))),
		FilePath:   strings.TrimSpace(getenv("INDEX_FILE_PATH")),
		S3Bucket:   strings.TrimSpace(getenv("INDEX_S3_BUCKET")),
		S3Key:      strings.TrimSpace(getenv("INDEX_S3_KEY")),
		S3Endpoint: strings.TrimSpace(getenv("INDEX_S3_ENDPOINT")),
	}
	if cfg.Mode == "" {
		cfg.Mode = ModeNone
	}
	if cfg.S3Key == "" {
		cfg.S3Key = DefaultS3Key
	}
	if err := cfg.validate(); err != nil {
		return Config{}, err
	}
	return cfg, nil
}

func (c Config) validate() error {
	switch c.Mode {
	case ModeNone:
	case ModeFilesystem:
		if c.FilePath == "" {
			return fmt.Errorf("indexstore: INDEX_FILE_PATH is required when INDEX_STORE=%s", ModeFilesystem)
		}
	case ModeS3:
		if c.S3Bucket == "" {
			return fmt.Errorf("indexstore: INDEX_S3_BUCKET is required when INDEX_STORE=%s", ModeS3)
		}
	default:
		return fmt.Errorf("indexstore: unknown INDEX_STORE %q (want none, filesystem, or s3)", string(c.Mode))
	}
	return nil
}

// New builds the store for cfg. It does not contact S3; an unreachable or
// missing bucket surfaces on the first Load or Save.
func New(ctx context.Context, cfg Config) (Store, error) {
	if cfg.Mode == "" {
		cfg.Mode = ModeNone
	}
	if err := cfg.validate(); err != nil {
		return nil, err
	}
	if cfg.FormatVersion == 0 {
		cfg.FormatVersion = DefaultFormatVersion
	}
	switch cfg.Mode {
	case ModeFilesystem:
		return newFileStore(cfg), nil
	case ModeS3:
		return newS3Store(ctx, cfg)
	default:
		return none{}, nil
	}
}
