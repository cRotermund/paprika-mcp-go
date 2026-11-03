package mirror

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/indexstore"
)

// SnapshotFormatVersion is the version of the snapshot payload below. Set it as
// indexstore.Config.FormatVersion. Change it whenever IndexEntry or
// snapshotPayload changes shape; an older snapshot is then ignored and costs
// one full load.
const SnapshotFormatVersion = 1

// snapshotPayload is the mirror's snapshot schema: the recipe index and when it
// was last verified by reading every recipe.
type snapshotPayload struct {
	// Complete is false for a partial index saved during the first load. Such
	// a snapshot is used to resume, but search stays unavailable until a pass finishes.
	Complete bool `json:"complete"`
	// FullReadAt is when every recipe was last read from the API.
	FullReadAt time.Time     `json:"full_read_at"`
	Recipes    []*IndexEntry `json:"recipes"`
}

// Snapshot builds the snapshot of the current index. It is the source for an
// indexstore.Saver.
func (m *Mirror) Snapshot(context.Context) (indexstore.Snapshot, error) {
	m.mu.RLock()
	p := snapshotPayload{
		Complete:   m.complete,
		FullReadAt: m.fullReadAt,
		Recipes:    make([]*IndexEntry, 0, len(m.index)),
	}
	for _, e := range m.index {
		p.Recipes = append(p.Recipes, e)
	}
	m.mu.RUnlock()

	raw, err := json.Marshal(p)
	if err != nil {
		return indexstore.Snapshot{}, err
	}
	return indexstore.Snapshot{Payload: raw}, nil
}

// LoadSnapshot loads the snapshot from the configured store. A missing
// snapshot is normal. An unusable one is logged and ignored, so the next pass
// is a full load. Call it before [Mirror.Sync]; [Mirror.Run] does.
func (m *Mirror) LoadSnapshot(ctx context.Context) {
	if m.store == nil {
		return
	}
	snap, found, err := m.store.Load(ctx)
	if err != nil {
		if errors.Is(err, indexstore.ErrUnusable) {
			m.log.Warn("ignoring unusable index snapshot; doing a full load", "error", err)
		} else {
			m.log.Warn("could not load index snapshot; doing a full load", "error", err)
		}
		return
	}
	if !found {
		m.log.Info("no index snapshot; doing a full load")
		return
	}
	var p snapshotPayload
	if err := json.Unmarshal(snap.Payload, &p); err != nil {
		m.log.Warn("ignoring unusable index snapshot; doing a full load", "error", err)
		return
	}

	entries := make([]*IndexEntry, 0, len(p.Recipes))
	for _, e := range p.Recipes {
		if e != nil && e.UID != "" {
			entries = append(entries, e)
		}
	}

	m.mu.Lock()
	for _, e := range entries {
		e.internWith(m.intern)
		m.index[e.UID] = e
	}
	m.complete = p.Complete
	m.ready = p.Complete
	m.fullReadAt = p.FullReadAt
	if m.fullReadAt.IsZero() && p.Complete {
		m.fullReadAt = snap.SavedAt
	}
	expired := m.expiredLocked()
	m.mu.Unlock()

	m.log.Info("index snapshot loaded", "recipes", len(entries), "complete", p.Complete,
		"saved_at", snap.SavedAt, "reread_all", expired)
}
