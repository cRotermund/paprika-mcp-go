package indexstore

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// DefaultSaveInterval is how often a changed index is saved at most.
const DefaultSaveInterval = 5 * time.Minute

const defaultSaveTimeout = 10 * time.Second

// SaverConfig tunes a Saver. Zero values select the defaults.
type SaverConfig struct {
	// Interval is the minimum time between save attempts. Default 5 minutes.
	Interval time.Duration
	// SaveTimeout bounds one save, including the final one at shutdown. Default 10 seconds.
	SaveTimeout time.Duration
	Logger      *slog.Logger
}

// Saver saves a snapshot in the background after the caller reports a change.
// It saves at most once per interval, retries failed saves at the next
// interval, and tries once more when Run's context is canceled.
type Saver struct {
	store    Store
	source   func(context.Context) (Snapshot, error)
	interval time.Duration
	timeout  time.Duration
	logger   *slog.Logger
	dirty    atomic.Bool
}

// NewSaver returns a Saver that writes to store whatever source returns. The
// source builds the snapshot payload; the Saver stamps SavedAt.
func NewSaver(store Store, source func(context.Context) (Snapshot, error), cfg SaverConfig) *Saver {
	if cfg.Interval <= 0 {
		cfg.Interval = DefaultSaveInterval
	}
	if cfg.SaveTimeout <= 0 {
		cfg.SaveTimeout = defaultSaveTimeout
	}
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	return &Saver{
		store:    store,
		source:   source,
		interval: cfg.Interval,
		timeout:  cfg.SaveTimeout,
		logger:   cfg.Logger,
	}
}

// MarkDirty records that the index changed. It never blocks.
func (s *Saver) MarkDirty() { s.dirty.Store(true) }

// Run blocks until ctx is canceled, then makes one last save attempt if the
// index changed since the last successful save.
func (s *Saver) Run(ctx context.Context) {
	ticker := time.NewTicker(s.interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			s.flush(context.WithoutCancel(ctx))
			return
		case <-ticker.C:
			s.flush(ctx)
		}
	}
}

func (s *Saver) flush(ctx context.Context) {
	if _, isNone := s.store.(none); isNone {
		return // nothing is stored in this mode, so there is nothing to save or log
	}
	if !s.dirty.Swap(false) {
		return
	}
	ctx, cancel := context.WithTimeout(ctx, s.timeout)
	defer cancel()
	snap, err := s.source(ctx)
	if err == nil {
		snap.SavedAt = time.Now()
		err = s.store.Save(ctx, snap)
	}
	if err != nil {
		s.dirty.Store(true)
		s.logger.Warn("index snapshot save failed; will retry", "error", err)
		return
	}
	s.logger.Debug("index snapshot saved")
}
