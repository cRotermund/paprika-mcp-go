package indexstore

import (
	"context"
	"errors"
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
)

type fileStore struct {
	path    string
	version int
}

func newFileStore(cfg Config) *fileStore {
	return &fileStore{path: cfg.FilePath, version: cfg.FormatVersion}
}

func (s *fileStore) Load(ctx context.Context) (Snapshot, bool, error) {
	if err := ctx.Err(); err != nil {
		return Snapshot{}, false, err
	}
	data, err := os.ReadFile(s.path)
	if errors.Is(err, fs.ErrNotExist) {
		return Snapshot{}, false, nil
	}
	if err != nil {
		return Snapshot{}, false, fmt.Errorf("indexstore: read snapshot file: %w", err)
	}
	snap, err := decode(data, s.version)
	if err != nil {
		return Snapshot{}, false, err
	}
	return snap, true, nil
}

// Save writes a temporary file in the same directory, syncs it, and renames it
// over the old snapshot, so a reader sees either the old or the new file.
func (s *fileStore) Save(ctx context.Context, snap Snapshot) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	data, err := encode(snap, s.version)
	if err != nil {
		return err
	}
	dir := filepath.Dir(s.path)
	if err := os.MkdirAll(dir, 0o700); err != nil {
		return fmt.Errorf("indexstore: create snapshot directory: %w", err)
	}
	tmp, err := os.CreateTemp(dir, filepath.Base(s.path)+".*.tmp")
	if err != nil {
		return fmt.Errorf("indexstore: create temporary file: %w", err)
	}
	tmpName := tmp.Name()
	committed := false
	defer func() {
		if !committed {
			_ = tmp.Close()
			_ = os.Remove(tmpName)
		}
	}()
	if err := tmp.Chmod(0o600); err != nil && !errors.Is(err, errors.ErrUnsupported) {
		return fmt.Errorf("indexstore: set file mode: %w", err)
	}
	if _, err := tmp.Write(data); err != nil {
		return fmt.Errorf("indexstore: write snapshot: %w", err)
	}
	if err := tmp.Sync(); err != nil {
		return fmt.Errorf("indexstore: sync snapshot: %w", err)
	}
	if err := tmp.Close(); err != nil {
		return fmt.Errorf("indexstore: close snapshot: %w", err)
	}
	if err := os.Rename(tmpName, s.path); err != nil {
		return fmt.Errorf("indexstore: replace snapshot: %w", err)
	}
	committed = true
	syncDir(dir)
	return nil
}

// syncDir makes the rename durable. Windows cannot open directories for
// syncing, so failures are tolerated: the snapshot is only an optimization.
func syncDir(dir string) {
	d, err := os.Open(dir)
	if err != nil {
		return
	}
	_ = d.Sync()
	_ = d.Close()
}
