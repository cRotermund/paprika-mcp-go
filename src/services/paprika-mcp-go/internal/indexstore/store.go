// Package indexstore saves the cache's recipe index snapshot to nothing, a local
// file, or an S3 object. It treats the snapshot as opaque and knows nothing about
// what it contains.
package indexstore

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"time"
)

// DefaultFormatVersion is the format version used when Config.FormatVersion
// is zero.
const DefaultFormatVersion = 1

// maxDecodedSize bounds the decompressed snapshot so a hostile or damaged
// object cannot exhaust memory.
const maxDecodedSize = 512 << 20

// ErrUnusable marks a stored snapshot that exists but cannot be used. Callers
// log it and fall back to a full load. ErrCorrupt and ErrVersionMismatch
// both match it with errors.Is.
var ErrUnusable = errors.New("indexstore: snapshot unusable")

var (
	// ErrCorrupt means the stored bytes are not a valid snapshot.
	ErrCorrupt = fmt.Errorf("%w: corrupt", ErrUnusable)
	// ErrVersionMismatch means the snapshot was written with another format version.
	ErrVersionMismatch = fmt.Errorf("%w: format version mismatch", ErrUnusable)
)

// Snapshot is what the store saves and loads. The store owns the envelope and
// compression; the caller owns the schema of Payload, which must be valid JSON.
type Snapshot struct {
	// Version is the format version found by Load. Save ignores it and
	// writes the store's configured format version.
	Version int
	// SavedAt is when the snapshot was saved. Save uses the current time when it is zero.
	SavedAt time.Time
	// Payload is the caller's data, stored verbatim.
	Payload json.RawMessage
}

// Store loads and saves one snapshot. An absent snapshot is not an error.
type Store interface {
	// Load returns found=false with a nil error when no snapshot exists. A
	// stored snapshot that cannot be used returns an error matching ErrUnusable.
	Load(ctx context.Context) (snap Snapshot, found bool, err error)
	// Save replaces the stored snapshot.
	Save(ctx context.Context, snap Snapshot) error
}

type envelope struct {
	Version int             `json:"version"`
	SavedAt time.Time       `json:"saved_at"`
	Payload json.RawMessage `json:"payload"`
}

func encode(snap Snapshot, version int) ([]byte, error) {
	if len(snap.Payload) == 0 {
		return nil, errors.New("indexstore: empty payload")
	}
	if snap.SavedAt.IsZero() {
		snap.SavedAt = time.Now()
	}
	var buf bytes.Buffer
	zw := gzip.NewWriter(&buf)
	enc := json.NewEncoder(zw)
	enc.SetEscapeHTML(false)
	err := enc.Encode(envelope{
		Version: version,
		SavedAt: snap.SavedAt.UTC(),
		Payload: snap.Payload,
	})
	if err != nil {
		return nil, fmt.Errorf("indexstore: encode snapshot: %w", err)
	}
	if err := zw.Close(); err != nil {
		return nil, fmt.Errorf("indexstore: compress snapshot: %w", err)
	}
	return buf.Bytes(), nil
}

func decode(data []byte, version int) (Snapshot, error) {
	zr, err := gzip.NewReader(bytes.NewReader(data))
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	defer zr.Close()
	raw, err := io.ReadAll(io.LimitReader(zr, maxDecodedSize+1))
	if err != nil {
		return Snapshot{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if len(raw) > maxDecodedSize {
		return Snapshot{}, fmt.Errorf("%w: decoded size exceeds limit", ErrCorrupt)
	}
	var env envelope
	if err := json.Unmarshal(raw, &env); err != nil {
		return Snapshot{}, fmt.Errorf("%w: %v", ErrCorrupt, err)
	}
	if env.Version == 0 || len(env.Payload) == 0 {
		return Snapshot{}, fmt.Errorf("%w: missing version or payload", ErrCorrupt)
	}
	if env.Version != version {
		return Snapshot{}, fmt.Errorf("%w: found %d, want %d", ErrVersionMismatch, env.Version, version)
	}
	return Snapshot{Version: env.Version, SavedAt: env.SavedAt, Payload: env.Payload}, nil
}

// none is the default mode: nothing is stored.
type none struct{}

func (none) Load(context.Context) (Snapshot, bool, error) { return Snapshot{}, false, nil }

func (none) Save(context.Context, Snapshot) error { return nil }
