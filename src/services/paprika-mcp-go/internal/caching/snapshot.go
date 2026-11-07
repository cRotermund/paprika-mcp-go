package caching

import (
	"context"
	"encoding/json"
	"errors"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/indexstore"
)

// SnapshotFormatVersion is the version of the snapshot payload below. Set it as
// indexstore.Config.FormatVersion. Change it whenever IndexEntry or
// snapshotPayload changes shape; an older snapshot is then ignored and costs
// one full load.
//
// Version 2 added IndexEntry.ReadAt and dropped the payload's full_read_at.
const SnapshotFormatVersion = 2

// snapshotPayload is the cache's snapshot schema: the recipe index, in which
// every entry records when its recipe was last read.
type snapshotPayload struct {
	// Complete is the cache's readiness when the snapshot was taken: false for
	// a partial index saved during the first load. Such a snapshot is used to
	// resume, but search stays unavailable until every recipe in the Paprika
	// list is indexed at its current hash.
	Complete bool          `json:"complete"`
	Recipes  []*IndexEntry `json:"recipes"`
}

// Snapshot builds the snapshot of the current index. It is the source for an
// indexstore.Saver.
func (c *Cache) Snapshot(context.Context) (indexstore.Snapshot, error) {
	c.mu.RLock()
	p := snapshotPayload{
		Complete: c.ready,
		Recipes:  make([]*IndexEntry, 0, len(c.index)),
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
	c.ready = p.Complete
	c.recountLocked()
	now := c.now()
	due := 0
	for _, e := range entries {
		if c.dueByAgeLocked(e, now) {
			due++
		}
	}
	c.mu.Unlock()

	c.log.Info(
		"index snapshot loaded",
		"recipes",
		len(entries),
		"complete",
		p.Complete,
		"saved_at",
		snap.SavedAt,
		"due_by_age",
		due,
	)
}
