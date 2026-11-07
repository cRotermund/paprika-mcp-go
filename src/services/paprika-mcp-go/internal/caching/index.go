package caching

import (
	"context"
	"errors"
	"slices"
	"time"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient"
)

// IndexEntry is what the cache keeps for every recipe: enough to search and to
// list, and nothing else. It holds no directions, notes, nutrition, or photo
// fields. Entries are immutable once stored. The JSON names are short because
// the snapshot holds thousands of them; they are part of the snapshot format.
type IndexEntry struct {
	UID        string   `json:"u"`
	Hash       string   `json:"h"`
	Name       string   `json:"n"`
	Categories []string `json:"c,omitempty"` // category uid values
	Rating     int      `json:"r,omitempty"`
	Favorite   bool     `json:"f,omitempty"`
	InTrash    bool     `json:"t,omitempty"`
	Source     string   `json:"s,omitempty"`
	Created    string   `json:"cr,omitempty"`
	PrepTime   string   `json:"pt,omitempty"`
	CookTime   string   `json:"ct,omitempty"`
	TotalTime  string   `json:"tt,omitempty"`
	// Tokens are the deduplicated, sorted search words of the ingredients.
	Tokens []string `json:"i,omitempty"`
}

// newIndexEntry builds an entry from a recipe. listHash, when not empty, is the
// hash sync/recipes/ reported for it and is stored in preference to the
// recipe's own, so that the next comparison with that list matches.
func newIndexEntry(r paprikaclient.Recipe, listHash string) *IndexEntry {
	hash := listHash
	if hash == "" {
		hash = r.Hash
	}
	return &IndexEntry{
		UID:        r.UID,
		Hash:       hash,
		Name:       r.Name,
		Categories: slices.Clone(r.Categories),
		Rating:     r.Rating,
		Favorite:   r.OnFavorites,
		InTrash:    r.InTrash,
		Source:     r.Source,
		Created:    r.Created,
		PrepTime:   r.PrepTime,
		CookTime:   r.CookTime,
		TotalTime:  r.TotalTime,
		Tokens:     ingredientTokens(r.Ingredients),
	}
}

// internWith replaces repeated strings with one shared copy each.
func (e *IndexEntry) internWith(table map[string]string) {
	share := func(s string) string {
		if s == "" {
			return s
		}
		if v, ok := table[s]; ok {
			return v
		}
		table[s] = s
		return s
	}
	for i, t := range e.Tokens {
		e.Tokens[i] = share(t)
	}
	for i, c := range e.Categories {
		e.Categories[i] = share(c)
	}
	e.Source = share(e.Source)
}

func (e *IndexEntry) equal(o *IndexEntry) bool {
	return e.UID == o.UID && e.Hash == o.Hash && e.Name == o.Name && e.Rating == o.Rating &&
		e.Favorite == o.Favorite && e.InTrash == o.InTrash && e.Source == o.Source &&
		e.Created == o.Created && e.PrepTime == o.PrepTime && e.CookTime == o.CookTime &&
		e.TotalTime == o.TotalTime && slices.Equal(e.Categories, o.Categories) &&
		slices.Equal(e.Tokens, o.Tokens)
}

// ensureRecipesFresh checks sync/status/ if due and, when the recipe counter
// moved (or a full re-read is due), runs a recipe pass. It does nothing before
// the startup pass has finished, and skips the pass if another one is running.
func (c *Cache) ensureRecipesFresh(ctx context.Context) error {
	counters, err := c.checkStatus(ctx)
	if err != nil {
		return err
	}
	c.mu.RLock()
	need := c.synced && (c.recipesCounter != counters[ResourceRecipes] || c.expiredLocked())
	c.mu.RUnlock()
	if !need || !c.syncMu.TryLock() {
		return nil
	}
	defer c.syncMu.Unlock()
	return c.syncRecipes(ctx)
}

// syncRecipes is one recipe pass. The caller holds syncMu.
//
// The status counter is read before the list, so a change that lands during
// the pass is seen as a counter difference at the next check.
func (c *Cache) syncRecipes(ctx context.Context) error {
	started := c.now()
	status, err := c.refreshStatus(ctx)
	if err != nil {
		return err
	}
	refs, err := c.src.ListRecipes(ctx)
	if err != nil {
		return err
	}

	c.mu.Lock()
	force := c.expiredLocked()
	seen := make(map[string]struct{}, len(refs))
	latest := make(map[string]string, len(refs))
	var todo []paprikaclient.RecipeRef
	for _, ref := range refs {
		if ref.UID == "" {
			continue
		}
		if _, dup := seen[ref.UID]; dup {
			continue
		}
		seen[ref.UID] = struct{}{}
		latest[ref.UID] = ref.Hash
		if cur, ok := c.index[ref.UID]; force || !ok || cur.Hash != ref.Hash {
			todo = append(todo, ref)
		}
	}
	var removed []string
	for uid := range c.index {
		if _, ok := seen[uid]; !ok {
			removed = append(removed, uid)
		}
	}
	for _, uid := range removed {
		delete(c.index, uid)
	}
	c.latest = latest
	c.recountLocked()
	justReady := c.checkReadyLocked()
	indexed := c.indexed
	full := len(todo) == len(seen)
	c.mu.Unlock()

	for _, uid := range removed {
		c.bodies.remove(uid)
	}
	if len(removed) > 0 || justReady {
		c.changed()
	}
	if len(todo) > 0 {
		c.log.Info(
			"reading recipes",
			"to_read",
			len(todo),
			"indexed",
			indexed,
			"total",
			len(latest),
			"removed",
			len(removed),
			"full_read",
			full,
		)
	}

	for _, ref := range todo {
		if err := ctx.Err(); err != nil {
			return err
		}
		body, err := c.src.GetRecipe(ctx, ref.UID)
		switch {
		case errors.Is(err, paprikaclient.ErrNotFound):
			c.forget(ref.UID)
		case err != nil:
			return err
		default:
			c.storeEntry(newIndexEntry(body.Value, ref.Hash))
		}
	}

	c.mu.Lock()
	firstSync := !c.synced
	c.synced = true
	c.recipesCounter = status[ResourceRecipes]
	if full {
		c.fullReadAt = c.now()
	}
	loaded := len(c.index)
	c.mu.Unlock()
	if firstSync {
		c.log.Info(
			"recipe index loaded",
			"recipes",
			loaded,
			"duration",
			c.now().Sub(started).Round(time.Millisecond),
		)
	}
	if full {
		c.changed()
	}
	return nil
}
