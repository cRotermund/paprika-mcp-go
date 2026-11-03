package caching

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

// snapshotPayload is the cache's snapshot schema: the recipe index and when it
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
func (c *Cache) Snapshot(context.Context) (indexstore.Snapshot, error) {
	c.mu.RLock()
	p := snapshotPayload{
		Complete:   c.complete,
		FullReadAt: c.fullReadAt,
		Recipes:    make([]*IndexEntry, 0, len(c.index)),
	}
	for _, e := range c.index {
		p.Recipes = append(p.Recipes, e)
	}
	c.mu.RUnlock()

	raw, err := json.Marshal(p)
	if err != nil {
		return indexstore.Snapshot{}, err
	}
	return indexstore.Snapshot{Payload: raw}, nil
}

// LoadSnapshot loads the snapshot from the configured store. A missing
// snapshot is normal. An unusable one is logged and ignored, so the next pass
// is a full load. Call it before [Cache.Sync]; [Cache.Run] does.
func (c *Cache) LoadSnapshot(ctx context.Context) {
	if c.store == nil {
		return
	}
	snap, found, err := c.store.Load(ctx)
	if err != nil {
		if errors.Is(err, indexstore.ErrUnusable) {
			c.log.Warn("ignoring unusable index snapshot; doing a full load", "error", err)
		} else {
			c.log.Warn("could not load index snapshot; doing a full load", "error", err)
		}
		return
	}
	if !found {
		c.log.Info("no index snapshot; doing a full load")
		return
	}
	var p snapshotPayload
	if err := json.Unmarshal(snap.Payload, &p); err != nil {
		c.log.Warn("ignoring unusable index snapshot; doing a full load", "error", err)
		return
	}

	entries := make([]*IndexEntry, 0, len(p.Recipes))
	for _, e := range p.Recipes {
		if e != nil && e.UID != "" {
			entries = append(entries, e)
		}
	}

	c.mu.Lock()
	for _, e := range entries {
		e.internWith(c.intern)
		c.index[e.UID] = e
	}
	c.complete = p.Complete
	c.ready = p.Complete
	c.fullReadAt = p.FullReadAt
	if c.fullReadAt.IsZero() && p.Complete {
		c.fullReadAt = snap.SavedAt
	}
	expired := c.expiredLocked()
	c.mu.Unlock()

	c.log.Info("index snapshot loaded", "recipes", len(entries), "complete", p.Complete,
		"saved_at", snap.SavedAt, "reread_all", expired)
}
