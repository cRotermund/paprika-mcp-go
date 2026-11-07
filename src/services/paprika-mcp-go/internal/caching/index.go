package caching

import (
	"context"
	"errors"
	"hash/fnv"
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
	// ReadAt is when the recipe body behind this entry was last read from
	// Paprika, possibly moved earlier by [readStamp]. The entry is due for a
	// re-read when it is older than the cache's expiry.
	ReadAt time.Time `json:"ra"`
}

// jitterDivisor sets the largest jitter: one tenth of the expiry.
const jitterDivisor = 10

// jitterFor returns the deterministic jitter for uid: a stable pseudo-random
// duration in [0, expiry/jitterDivisor) derived from an FNV-1a hash of the uid.
func jitterFor(uid string, expiry time.Duration) time.Duration {
	span := uint64(expiry / jitterDivisor)
	if span == 0 {
		return 0
	}
	h := fnv.New64a()
	_, _ = h.Write([]byte(uid))
	return time.Duration(h.Sum64() % span)
}

// readStamp is the ReadAt to record for a recipe read at now. When spread is
// true the stamp is moved earlier by the uid's jitter. Entries loaded in one
// burst (a first load) or re-read in one pass would otherwise all carry nearly
// the same ReadAt and fall due on the same day, so every cycle would be another
// burst against Paprika's throttle. The jitter makes each entry fall due up to
// 10% of the expiry early, and by a different amount, so due dates spread out
// and stay spread. Hash-driven reads and writes use the plain read time, since
// their timing is already scattered.
func (c *Cache) readStamp(uid string, now time.Time, spread bool) time.Time {
	if !spread {
		return now
	}
	return now.Add(-jitterFor(uid, c.expiry))
}

// dueByAgeLocked reports whether e was last read more than the expiry ago. The
// caller holds mu.
func (c *Cache) dueByAgeLocked(e *IndexEntry, now time.Time) bool {
	return now.Sub(e.ReadAt) > c.expiry
}

// anyDueByAgeLocked reports whether any entry is due by age. It scans the
// index, which costs microseconds for thousands of entries and no request. The
// caller holds mu.
func (c *Cache) anyDueByAgeLocked() bool {
	now := c.now()
	for _, e := range c.index {
		if c.dueByAgeLocked(e, now) {
			return true
		}
	}
	return false
}

// newIndexEntry builds an entry from a recipe read at readAt. listHash, when
// not empty, is the hash sync/recipes/ reported for it and is stored in
// preference to the recipe's own, so that the next comparison with that list
// matches.
func newIndexEntry(r paprikaclient.Recipe, listHash string, readAt time.Time) *IndexEntry {
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
		ReadAt:     readAt,
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
		slices.Equal(e.Tokens, o.Tokens) && e.ReadAt.Equal(o.ReadAt)
}

// ensureRecipesFresh checks sync/status/ if due and, when the recipe counter
// moved or any entry is due for an age-based re-read, runs a recipe pass. A
// pass wanted only for age (counter unchanged) starts at most once per FreshFor,
// measured from the start of the previous pass, so failing re-reads cannot
// make every read start another pass. It does nothing before the startup pass
// has finished, and skips the pass if another one is running.
func (c *Cache) ensureRecipesFresh(ctx context.Context) error {
	counters, err := c.checkStatus(ctx)
	if err != nil {
		return err
	}
	c.mu.RLock()
	need := c.synced && (c.recipesCounter != counters[ResourceRecipes] ||
		(c.now().Sub(c.passStartedAt) >= c.freshFor && c.anyDueByAgeLocked()))
	c.mu.RUnlock()
	if !need || !c.syncMu.TryLock() {
		return nil
	}
	defer c.syncMu.Unlock()
	return c.syncRecipes(ctx)
}

// readJob is one recipe a pass will read.
type readJob struct {
	ref paprikaclient.RecipeRef
	// spread asks for the read to be stamped with the uid's jitter.
	spread bool
	// readAt is the ReadAt the index held before, for ordering age-based jobs.
	readAt time.Time
}

// syncRecipes is one recipe pass. The caller holds syncMu.
//
// A recipe is due when it is new, when its hash differs from the list's, or
// when its ReadAt is older than the expiry. The pass reads the first two kinds
// (they decide readiness) in list order, then the age-based ones, oldest
// ReadAt first, so that an interrupted pass loses nothing and the next one
// resumes with whatever is still due.
//
// The status counter is read before the list, so a change that lands during
// the pass is seen as a counter difference at the next check.
func (c *Cache) syncRecipes(ctx context.Context) error {
	started := c.now()
	c.mu.Lock()
	c.passStartedAt = started
	c.mu.Unlock()
	status, err := c.refreshStatus(ctx)
	if err != nil {
		return err
	}
	refs, err := c.src.ListRecipes(ctx)
	if err != nil {
		return err
	}

	c.mu.Lock()
	seen := make(map[string]struct{}, len(refs))
	latest := make(map[string]string, len(refs))
	var changed, aged []readJob
	for _, ref := range refs {
		if ref.UID == "" {
			continue
		}
		if _, dup := seen[ref.UID]; dup {
			continue
		}
		seen[ref.UID] = struct{}{}
		latest[ref.UID] = ref.Hash
		cur, ok := c.index[ref.UID]
		switch {
		case !ok:
			changed = append(changed, readJob{ref: ref, spread: true})
		case cur.Hash != ref.Hash:
			changed = append(changed, readJob{ref: ref})
		case c.dueByAgeLocked(cur, started):
			aged = append(aged, readJob{ref: ref, spread: true, readAt: cur.ReadAt})
		}
	}
	slices.SortStableFunc(aged, func(a, b readJob) int { return a.readAt.Compare(b.readAt) })
	todo := append(changed, aged...)
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
			"new_or_changed",
			len(changed),
			"aged",
			len(aged),
			"indexed",
			indexed,
			"total",
			len(latest),
			"removed",
			len(removed),
		)
	}

	for _, job := range todo {
		if err := ctx.Err(); err != nil {
			return err
		}
		body, err := c.src.GetRecipe(ctx, job.ref.UID)
		switch {
		case errors.Is(err, paprikaclient.ErrNotFound):
			c.forget(job.ref.UID)
		case err != nil:
			return err
		default:
			readAt := c.readStamp(job.ref.UID, c.now(), job.spread)
			c.storeEntry(newIndexEntry(body.Value, job.ref.Hash, readAt))
		}
	}

	c.mu.Lock()
	firstSync := !c.synced
	c.synced = true
	c.recipesCounter = status[ResourceRecipes]
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
	return nil
}
