package caching

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"sync"
	"sync/atomic"
	"time"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/indexstore"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient"
)

// Defaults for [Options] fields left at their zero value.
const (
	DefaultFreshFor = 60 * time.Second
	DefaultExpiry   = 30 * 24 * time.Hour
	DefaultRetryMin = 5 * time.Second
	DefaultRetryMax = 5 * time.Minute
)

// Resource names, matching the sync/status/ counters and the API routes.
const (
	ResourceRecipes            = "recipes"
	ResourceCategories         = "categories"
	ResourceMeals              = "meals"
	ResourceMealTypes          = "mealtypes"
	ResourceGroceryLists       = "grocerylists"
	ResourceGroceries          = "groceries"
	ResourceGroceryAisles      = "groceryaisles"
	ResourceGroceryIngredients = "groceryingredients"
)

// Source is the part of paprikaclient.Client that the cache reads from.
type Source interface {
	Status(ctx context.Context) (paprikaclient.Counters, error)
	ListRecipes(ctx context.Context) ([]paprikaclient.RecipeRef, error)
	GetRecipe(ctx context.Context, uid string) (paprikaclient.Entry[paprikaclient.Recipe], error)
	Categories(ctx context.Context) ([]paprikaclient.Entry[paprikaclient.Category], error)
	Meals(ctx context.Context) ([]paprikaclient.Entry[paprikaclient.Meal], error)
	MealTypes(ctx context.Context) ([]paprikaclient.Entry[paprikaclient.MealType], error)
	GroceryLists(ctx context.Context) ([]paprikaclient.Entry[paprikaclient.GroceryList], error)
	Groceries(ctx context.Context) ([]paprikaclient.Entry[paprikaclient.Grocery], error)
	GroceryAisles(ctx context.Context) ([]paprikaclient.Entry[paprikaclient.GroceryAisle], error)
	GroceryIngredients(ctx context.Context) (
		[]paprikaclient.Entry[paprikaclient.GroceryIngredient],
		error,
	)
}

// Options configures [New]. Zero values select the defaults.
type Options struct {
	Logger *slog.Logger
	// Store is where [Cache.Run] loads the snapshot from. Nil means none.
	Store indexstore.Store
	// FreshFor is how long a sync/status/ answer is trusted.
	FreshFor time.Duration
	// Expiry is how long recipes may go without a full re-read.
	Expiry time.Duration
	// BodyCacheSize is the number of recipe bodies kept.
	BodyCacheSize int
	// RetryMin and RetryMax bound the wait between failed startup passes.
	RetryMin, RetryMax time.Duration
	// Now replaces time.Now. Tests use it.
	Now func() time.Time
}

// LoadingError means the recipe index is not ready for search yet.
type LoadingError struct {
	// Indexed is the number of recipes in the latest Paprika list that are in
	// the index at their current hash. Total is the size of that list, and is
	// zero until the list has been read.
	Indexed, Total int
}

func (e *LoadingError) Error() string {
	if e.Total == 0 {
		return "recipe index is still loading"
	}
	return fmt.Sprintf(
		"recipe index is still loading (%d of %d recipes read)",
		e.Indexed,
		e.Total,
	)
}

// Progress reports the state of the recipe index.
type Progress struct {
	// Ready is true when search can be served. It becomes true once every
	// recipe in the latest Paprika list is indexed at its current hash, however
	// many passes that took, and then stays true.
	Ready bool
	// Indexed is the number of recipes in the latest Paprika list that are in
	// the index at their current hash. Total is the size of that list, and is
	// zero until the list has been read.
	Indexed, Total int
	// Recipes is the number of entries in the index.
	Recipes int
}

// Cache holds disposable local copies of Paprika data: the recipe index, recent
// recipe bodies, and the small lists. It is safe for concurrent use.
type Cache struct {
	src      Source
	log      *slog.Logger
	store    indexstore.Store
	freshFor time.Duration
	expiry   time.Duration
	retryMin time.Duration
	retryMax time.Duration
	now      func() time.Time

	onChange atomic.Pointer[func()]

	mu             sync.RWMutex
	index          map[string]*IndexEntry
	intern         map[string]string
	ready          bool              // search can be served; once true, it stays true
	synced         bool              // a recipe pass has succeeded in this process
	latest         map[string]string // uid to hash, latest list; nil before one is read
	indexed        int               // entries of latest in the index at the listed hash
	fullReadAt     time.Time
	recipesCounter int64 // counter at the last successful pass, or -1

	syncMu sync.Mutex // one recipe pass at a time

	statusMu  sync.Mutex
	counters  paprikaclient.Counters
	lastCheck time.Time

	bodies *bodyCache

	categories  *resource[paprikaclient.Category]
	meals       *resource[paprikaclient.Meal]
	mealTypes   *resource[paprikaclient.MealType]
	lists       *resource[paprikaclient.GroceryList]
	groceries   *resource[paprikaclient.Grocery]
	aisles      *resource[paprikaclient.GroceryAisle]
	ingredients *resource[paprikaclient.GroceryIngredient]
	byName      map[string]invalidator
}

// New returns a Cache that reads from src. It makes no requests; call
// [Cache.Run] to start loading.
func New(src Source, opts Options) *Cache {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.FreshFor <= 0 {
		opts.FreshFor = DefaultFreshFor
	}
	if opts.Expiry <= 0 {
		opts.Expiry = DefaultExpiry
	}
	if opts.RetryMin <= 0 {
		opts.RetryMin = DefaultRetryMin
	}
	if opts.RetryMax < opts.RetryMin {
		opts.RetryMax = max(DefaultRetryMax, opts.RetryMin)
	}
	if opts.Now == nil {
		opts.Now = time.Now
	}
	c := &Cache{
		src:            src,
		log:            opts.Logger,
		store:          opts.Store,
		freshFor:       opts.FreshFor,
		expiry:         opts.Expiry,
		retryMin:       opts.RetryMin,
		retryMax:       opts.RetryMax,
		now:            opts.Now,
		index:          map[string]*IndexEntry{},
		intern:         map[string]string{},
		recipesCounter: -1,
		bodies:         newBodyCache(opts.BodyCacheSize),
	}
	c.categories = &resource[paprikaclient.Category]{name: ResourceCategories, load: src.Categories}
	c.meals = &resource[paprikaclient.Meal]{name: ResourceMeals, load: src.Meals}
	c.mealTypes = &resource[paprikaclient.MealType]{name: ResourceMealTypes, load: src.MealTypes}
	c.lists = &resource[paprikaclient.GroceryList]{
		name: ResourceGroceryLists,
		load: src.GroceryLists,
	}
	c.groceries = &resource[paprikaclient.Grocery]{name: ResourceGroceries, load: src.Groceries}
	c.aisles = &resource[paprikaclient.GroceryAisle]{
		name: ResourceGroceryAisles,
		load: src.GroceryAisles,
	}
	c.ingredients = &resource[paprikaclient.GroceryIngredient]{
		name: ResourceGroceryIngredients,
		load: src.GroceryIngredients,
	}
	c.byName = map[string]invalidator{}
	for _, r := range []invalidator{
		c.categories,
		c.meals,
		c.mealTypes,
		c.lists,
		c.groceries,
		c.aisles,
		c.ingredients,
	} {
		c.byName[r.resourceName()] = r
	}
	return c
}

// SetOnChange registers a function called whenever the recipe index changes,
// such as indexstore.Saver.MarkDirty. It must not block.
func (c *Cache) SetOnChange(fn func()) {
	c.onChange.Store(&fn)
}

func (c *Cache) changed() {
	if fn := c.onChange.Load(); fn != nil {
		(*fn)()
	}
}

// Progress reports whether search is ready and how far loading has got.
func (c *Cache) Progress() Progress {
	c.mu.RLock()
	defer c.mu.RUnlock()
	return Progress{
		Ready:   c.ready,
		Indexed: c.indexed,
		Total:   len(c.latest),
		Recipes: len(c.index),
	}
}

// Run loads the snapshot, then reads and compares the recipe list, fetching
// what differs, and retries with backoff until a pass succeeds or ctx ends. It
// blocks, so start it in a goroutine.
func (c *Cache) Run(ctx context.Context) {
	c.LoadSnapshot(ctx)
	wait := c.retryMin
	for {
		err := c.Sync(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		if errors.Is(err, paprikaclient.ErrUnauthorized) {
			c.log.Error(
				"recipe sync cannot sign in to Paprika; check the server's credentials",
				"retry_in",
				wait,
			)
		} else {
			c.log.Warn("recipe sync failed; will retry", "error", err, "retry_in", wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, c.retryMax)
	}
}

// Sync runs one pass over the recipe list: it reads sync/recipes/, compares
// every uid and hash with the index, fetches new and changed recipes, and
// drops removed ones. It waits if another pass is running.
func (c *Cache) Sync(ctx context.Context) error {
	c.syncMu.Lock()
	defer c.syncMu.Unlock()
	return c.syncRecipes(ctx)
}

// Fresh makes the cache current: it checks sync/status/ when the last check is
// old, and applies recipe changes. List resources reload on their next read.
// An error leaves whatever the cache already holds in place.
func (c *Cache) Fresh(ctx context.Context) error {
	return c.ensureRecipesFresh(ctx)
}

// Invalidate marks a resource as changed so the next read downloads it again,
// and makes the next read check sync/status/. It reports false for an unknown
// resource name. For [ResourceRecipes] the next read re-compares the recipe list.
func (c *Cache) Invalidate(resource string) bool {
	if resource == ResourceRecipes {
		c.mu.Lock()
		c.recipesCounter = -1
		c.mu.Unlock()
	} else if r, ok := c.byName[resource]; ok {
		r.invalidate()
	} else {
		return false
	}
	c.statusMu.Lock()
	c.lastCheck = time.Time{}
	c.statusMu.Unlock()
	return true
}

// Refresh invalidates a resource and downloads it again now.
func (c *Cache) Refresh(ctx context.Context, resource string) error {
	if !c.Invalidate(resource) {
		return fmt.Errorf("caching: unknown resource %q", resource)
	}
	if resource == ResourceRecipes {
		return c.Sync(ctx)
	}
	return c.byName[resource].ensure(ctx, c)
}

// checkStatus returns the change counters, calling sync/status/ only when the
// last answer is older than FreshFor.
func (c *Cache) checkStatus(ctx context.Context) (paprikaclient.Counters, error) {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	if c.counters != nil && !c.lastCheck.IsZero() && c.now().Sub(c.lastCheck) < c.freshFor {
		return c.counters, nil
	}
	return c.fetchStatusLocked(ctx)
}

// refreshStatus calls sync/status/ regardless of age.
func (c *Cache) refreshStatus(ctx context.Context) (paprikaclient.Counters, error) {
	c.statusMu.Lock()
	defer c.statusMu.Unlock()
	return c.fetchStatusLocked(ctx)
}

func (c *Cache) fetchStatusLocked(ctx context.Context) (paprikaclient.Counters, error) {
	counters, err := c.src.Status(ctx)
	if err != nil {
		return nil, err
	}
	c.counters = counters
	c.lastCheck = c.now()
	return counters, nil
}

type invalidator interface {
	resourceName() string
	invalidate()
	ensure(ctx context.Context, c *Cache) error
}

// resource is a whole small list that is downloaded again only when its
// counter changes.
type resource[T any] struct {
	name string
	load func(context.Context) ([]paprikaclient.Entry[T], error)

	mu      sync.Mutex // serializes loads and guards the fields below
	entries []paprikaclient.Entry[T]
	loaded  bool
	counter int64
}

func (r *resource[T]) resourceName() string { return r.name }

func (r *resource[T]) invalidate() {
	r.mu.Lock()
	r.loaded = false
	r.mu.Unlock()
}

func (r *resource[T]) ensure(ctx context.Context, c *Cache) error {
	_, err := r.get(ctx, c)
	return err
}

// get returns the list, downloading it first when it is missing or its counter
// changed. If Paprika cannot be reached, a list already held is returned.
func (r *resource[T]) get(ctx context.Context, c *Cache) ([]paprikaclient.Entry[T], error) {
	counters, statusErr := c.checkStatus(ctx)

	r.mu.Lock()
	defer r.mu.Unlock()
	if statusErr != nil {
		if r.loaded {
			c.log.Warn(
				"could not check for changes; serving held list",
				"resource",
				r.name,
				"error",
				statusErr,
			)
			return r.entries, nil
		}
		return nil, statusErr
	}
	want := counters[r.name]
	if r.loaded && r.counter == want {
		return r.entries, nil
	}
	entries, err := r.load(ctx)
	if err != nil {
		if r.loaded {
			c.log.Warn("could not reload; serving held list", "resource", r.name, "error", err)
			return r.entries, nil
		}
		return nil, err
	}
	r.entries, r.loaded, r.counter = entries, true, want
	return entries, nil
}

// Categories returns every recipe category.
func (c *Cache) Categories(ctx context.Context) (
	[]paprikaclient.Entry[paprikaclient.Category],
	error,
) {
	return c.categories.get(ctx, c)
}

// Meals returns every planned meal.
func (c *Cache) Meals(ctx context.Context) ([]paprikaclient.Entry[paprikaclient.Meal], error) {
	return c.meals.get(ctx, c)
}

// MealTypes returns the account's meal types.
func (c *Cache) MealTypes(ctx context.Context) (
	[]paprikaclient.Entry[paprikaclient.MealType],
	error,
) {
	return c.mealTypes.get(ctx, c)
}

// GroceryLists returns every grocery list.
func (c *Cache) GroceryLists(ctx context.Context) (
	[]paprikaclient.Entry[paprikaclient.GroceryList],
	error,
) {
	return c.lists.get(ctx, c)
}

// Groceries returns every grocery item on every list.
func (c *Cache) Groceries(ctx context.Context) (
	[]paprikaclient.Entry[paprikaclient.Grocery],
	error,
) {
	return c.groceries.get(ctx, c)
}

// GroceryAisles returns every store aisle.
func (c *Cache) GroceryAisles(ctx context.Context) (
	[]paprikaclient.Entry[paprikaclient.GroceryAisle],
	error,
) {
	return c.aisles.get(ctx, c)
}

// GroceryIngredients returns the remembered ingredient-to-aisle choices.
func (c *Cache) GroceryIngredients(ctx context.Context) (
	[]paprikaclient.Entry[paprikaclient.GroceryIngredient],
	error,
) {
	return c.ingredients.get(ctx, c)
}

// Recipe returns one full recipe. A body already held is used only if its hash
// equals the index's hash. Otherwise, and always while the index is loading, the
// recipe is read from the API. A recipe Paprika does not have is
// [paprikaclient.ErrNotFound].
func (c *Cache) Recipe(
	ctx context.Context,
	uid string,
) (paprikaclient.Entry[paprikaclient.Recipe], error) {
	if c.Progress().Ready {
		if err := c.ensureRecipesFresh(ctx); err != nil {
			c.log.Warn("could not check for recipe changes; serving held data", "error", err)
		}
		if e, ok := c.IndexEntry(uid); ok {
			if body, hit := c.bodies.get(uid, e.Hash); hit {
				return body, nil
			}
		}
	}
	body, err := c.src.GetRecipe(ctx, uid)
	if err != nil {
		if errors.Is(err, paprikaclient.ErrNotFound) {
			c.forget(uid)
		}
		return body, err
	}
	c.UpsertRecipe(body)
	return body, nil
}

// UpsertRecipe stores a recipe as read or written by the caller: it updates the
// index entry and the body cache. The write path calls it after a successful
// save, with the object it sent.
func (c *Cache) UpsertRecipe(body paprikaclient.Entry[paprikaclient.Recipe]) {
	if body.Value.UID == "" {
		return
	}
	c.bodies.put(body)
	c.storeEntry(newIndexEntry(body.Value, ""))
}

func (c *Cache) forget(uid string) {
	c.bodies.remove(uid)
	c.mu.Lock()
	_, had := c.index[uid]
	c.setLocked(uid, nil)
	// The recipe is gone, so the list no longer waits for it.
	delete(c.latest, uid)
	ready := c.checkReadyLocked()
	c.mu.Unlock()
	if had || ready {
		c.changed()
	}
}

// IndexEntry returns the index entry for uid. The entry is shared and must not
// be modified.
func (c *Cache) IndexEntry(uid string) (*IndexEntry, bool) {
	c.mu.RLock()
	defer c.mu.RUnlock()
	e, ok := c.index[uid]
	return e, ok
}

// RecipeName returns the indexed name of a recipe, or "" when it is unknown.
func (c *Cache) RecipeName(uid string) string {
	if e, ok := c.IndexEntry(uid); ok {
		return e.Name
	}
	return ""
}

// storeEntry puts e in the index, sharing repeated strings, and reports a change.
func (c *Cache) storeEntry(e *IndexEntry) {
	c.mu.Lock()
	e.internWith(c.intern)
	old, had := c.index[e.UID]
	c.setLocked(e.UID, e)
	ready := c.checkReadyLocked()
	c.mu.Unlock()
	if !had || !old.equal(e) || ready {
		c.changed()
	}
}

// setLocked stores e as the entry for uid, or removes the entry when e is nil,
// and keeps the indexed count in step. The caller holds mu.
func (c *Cache) setLocked(uid string, e *IndexEntry) {
	if c.matchesLocked(uid, c.index[uid]) {
		c.indexed--
	}
	if e == nil {
		delete(c.index, uid)
		return
	}
	c.index[uid] = e
	if c.matchesLocked(uid, e) {
		c.indexed++
	}
}

// matchesLocked reports whether e is the entry for uid at the hash in the
// latest recipe list. The caller holds mu.
func (c *Cache) matchesLocked(uid string, e *IndexEntry) bool {
	want, listed := c.latest[uid]
	return listed && e != nil && e.Hash == want
}

// recountLocked recomputes the indexed count after the index or the latest
// list was replaced wholesale. The caller holds mu.
func (c *Cache) recountLocked() {
	c.indexed = 0
	for uid, e := range c.index {
		if c.matchesLocked(uid, e) {
			c.indexed++
		}
	}
}

// checkReadyLocked makes the cache ready once every recipe in the latest list
// is indexed at its listed hash, and reports whether it just did. Readiness is
// never withdrawn. The caller holds mu.
func (c *Cache) checkReadyLocked() bool {
	if c.ready || c.latest == nil || c.indexed != len(c.latest) {
		return false
	}
	c.ready = true
	return true
}

// expiredLocked reports whether recipes are due for a full re-read. The caller
// holds mu.
func (c *Cache) expiredLocked() bool {
	return !c.fullReadAt.IsZero() && c.now().Sub(c.fullReadAt) > c.expiry
}
