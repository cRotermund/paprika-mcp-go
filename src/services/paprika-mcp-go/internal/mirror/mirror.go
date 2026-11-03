package mirror

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

// Source is the part of paprikaclient.Client that the mirror reads from.
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
	GroceryIngredients(ctx context.Context) ([]paprikaclient.Entry[paprikaclient.GroceryIngredient], error)
}

// Options configures [New]. Zero values select the defaults.
type Options struct {
	Logger *slog.Logger
	// Store is where [Mirror.Run] loads the snapshot from. Nil means none.
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
	// Done and Total count the recipes read so far in the current pass.
	// Total is zero until the recipe list has been read.
	Done, Total int
}

func (e *LoadingError) Error() string {
	if e.Total == 0 {
		return "recipe index is still loading"
	}
	return fmt.Sprintf("recipe index is still loading (%d of %d recipes read)", e.Done, e.Total)
}

// Progress reports the state of the recipe index.
type Progress struct {
	// Ready is true when search can be served.
	Ready bool
	// Done and Total count the recipes read in the current pass.
	Done, Total int
	// Recipes is the number of entries in the index.
	Recipes int
}

// Mirror is the local view of the Paprika library. It is safe for concurrent use.
type Mirror struct {
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
	ready          bool // search can be served
	complete       bool // the index once covered the whole recipe list
	synced         bool // a recipe pass has succeeded in this process
	done, total    int
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

// New returns a Mirror that reads from src. It makes no requests; call
// [Mirror.Run] to start loading.
func New(src Source, opts Options) *Mirror {
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
	m := &Mirror{
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
	m.categories = &resource[paprikaclient.Category]{name: ResourceCategories, load: src.Categories}
	m.meals = &resource[paprikaclient.Meal]{name: ResourceMeals, load: src.Meals}
	m.mealTypes = &resource[paprikaclient.MealType]{name: ResourceMealTypes, load: src.MealTypes}
	m.lists = &resource[paprikaclient.GroceryList]{name: ResourceGroceryLists, load: src.GroceryLists}
	m.groceries = &resource[paprikaclient.Grocery]{name: ResourceGroceries, load: src.Groceries}
	m.aisles = &resource[paprikaclient.GroceryAisle]{name: ResourceGroceryAisles, load: src.GroceryAisles}
	m.ingredients = &resource[paprikaclient.GroceryIngredient]{name: ResourceGroceryIngredients, load: src.GroceryIngredients}
	m.byName = map[string]invalidator{}
	for _, r := range []invalidator{m.categories, m.meals, m.mealTypes, m.lists, m.groceries, m.aisles, m.ingredients} {
		m.byName[r.resourceName()] = r
	}
	return m
}

// SetOnChange registers a function called whenever the recipe index changes,
// such as indexstore.Saver.MarkDirty. It must not block.
func (m *Mirror) SetOnChange(fn func()) {
	m.onChange.Store(&fn)
}

func (m *Mirror) changed() {
	if fn := m.onChange.Load(); fn != nil {
		(*fn)()
	}
}

// Progress reports whether search is ready and how far loading has got.
func (m *Mirror) Progress() Progress {
	m.mu.RLock()
	defer m.mu.RUnlock()
	return Progress{Ready: m.ready, Done: m.done, Total: m.total, Recipes: len(m.index)}
}

// Run loads the snapshot, then reads and compares the recipe list, fetching
// what differs, and retries with backoff until a pass succeeds or ctx ends. It
// blocks, so start it in a goroutine.
func (m *Mirror) Run(ctx context.Context) {
	m.LoadSnapshot(ctx)
	wait := m.retryMin
	for {
		err := m.Sync(ctx)
		if err == nil || ctx.Err() != nil {
			return
		}
		if errors.Is(err, paprikaclient.ErrUnauthorized) {
			m.log.Error("recipe sync cannot sign in to Paprika; check the server's credentials", "retry_in", wait)
		} else {
			m.log.Warn("recipe sync failed; will retry", "error", err, "retry_in", wait)
		}
		select {
		case <-ctx.Done():
			return
		case <-time.After(wait):
		}
		wait = min(wait*2, m.retryMax)
	}
}

// Sync runs one pass over the recipe list: it reads sync/recipes/, compares
// every uid and hash with the index, fetches new and changed recipes, and
// drops removed ones. It waits if another pass is running.
func (m *Mirror) Sync(ctx context.Context) error {
	m.syncMu.Lock()
	defer m.syncMu.Unlock()
	return m.syncRecipes(ctx)
}

// Fresh makes the mirror current: it checks sync/status/ when the last check is
// old, and applies recipe changes. List resources reload on their next read.
// An error leaves whatever the mirror already holds in place.
func (m *Mirror) Fresh(ctx context.Context) error {
	return m.ensureRecipesFresh(ctx)
}

// Invalidate marks a resource as changed so the next read downloads it again,
// and makes the next read check sync/status/. It reports false for an unknown
// resource name. For [ResourceRecipes] the next read re-compares the recipe list.
func (m *Mirror) Invalidate(resource string) bool {
	if resource == ResourceRecipes {
		m.mu.Lock()
		m.recipesCounter = -1
		m.mu.Unlock()
	} else if r, ok := m.byName[resource]; ok {
		r.invalidate()
	} else {
		return false
	}
	m.statusMu.Lock()
	m.lastCheck = time.Time{}
	m.statusMu.Unlock()
	return true
}

// Refresh invalidates a resource and downloads it again now.
func (m *Mirror) Refresh(ctx context.Context, resource string) error {
	if !m.Invalidate(resource) {
		return fmt.Errorf("mirror: unknown resource %q", resource)
	}
	if resource == ResourceRecipes {
		return m.Sync(ctx)
	}
	return m.byName[resource].ensure(ctx, m)
}

// checkStatus returns the change counters, calling sync/status/ only when the
// last answer is older than FreshFor.
func (m *Mirror) checkStatus(ctx context.Context) (paprikaclient.Counters, error) {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	if m.counters != nil && !m.lastCheck.IsZero() && m.now().Sub(m.lastCheck) < m.freshFor {
		return m.counters, nil
	}
	return m.fetchStatusLocked(ctx)
}

// refreshStatus calls sync/status/ regardless of age.
func (m *Mirror) refreshStatus(ctx context.Context) (paprikaclient.Counters, error) {
	m.statusMu.Lock()
	defer m.statusMu.Unlock()
	return m.fetchStatusLocked(ctx)
}

func (m *Mirror) fetchStatusLocked(ctx context.Context) (paprikaclient.Counters, error) {
	c, err := m.src.Status(ctx)
	if err != nil {
		return nil, err
	}
	m.counters = c
	m.lastCheck = m.now()
	return c, nil
}

type invalidator interface {
	resourceName() string
	invalidate()
	ensure(ctx context.Context, m *Mirror) error
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

func (r *resource[T]) ensure(ctx context.Context, m *Mirror) error {
	_, err := r.get(ctx, m)
	return err
}

// get returns the list, downloading it first when it is missing or its counter
// changed. If Paprika cannot be reached, a list already held is returned.
func (r *resource[T]) get(ctx context.Context, m *Mirror) ([]paprikaclient.Entry[T], error) {
	counters, statusErr := m.checkStatus(ctx)

	r.mu.Lock()
	defer r.mu.Unlock()
	if statusErr != nil {
		if r.loaded {
			m.log.Warn("could not check for changes; serving held list", "resource", r.name, "error", statusErr)
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
			m.log.Warn("could not reload; serving held list", "resource", r.name, "error", err)
			return r.entries, nil
		}
		return nil, err
	}
	r.entries, r.loaded, r.counter = entries, true, want
	return entries, nil
}

// Categories returns every recipe category.
func (m *Mirror) Categories(ctx context.Context) ([]paprikaclient.Entry[paprikaclient.Category], error) {
	return m.categories.get(ctx, m)
}

// Meals returns every planned meal.
func (m *Mirror) Meals(ctx context.Context) ([]paprikaclient.Entry[paprikaclient.Meal], error) {
	return m.meals.get(ctx, m)
}

// MealTypes returns the account's meal types.
func (m *Mirror) MealTypes(ctx context.Context) ([]paprikaclient.Entry[paprikaclient.MealType], error) {
	return m.mealTypes.get(ctx, m)
}

// GroceryLists returns every grocery list.
func (m *Mirror) GroceryLists(ctx context.Context) ([]paprikaclient.Entry[paprikaclient.GroceryList], error) {
	return m.lists.get(ctx, m)
}

// Groceries returns every grocery item on every list.
func (m *Mirror) Groceries(ctx context.Context) ([]paprikaclient.Entry[paprikaclient.Grocery], error) {
	return m.groceries.get(ctx, m)
}

// GroceryAisles returns every store aisle.
func (m *Mirror) GroceryAisles(ctx context.Context) ([]paprikaclient.Entry[paprikaclient.GroceryAisle], error) {
	return m.aisles.get(ctx, m)
}

// GroceryIngredients returns the remembered ingredient-to-aisle choices.
func (m *Mirror) GroceryIngredients(ctx context.Context) ([]paprikaclient.Entry[paprikaclient.GroceryIngredient], error) {
	return m.ingredients.get(ctx, m)
}

// Recipe returns one full recipe. A body already held is used only if its hash
// equals the index's hash. Otherwise, and always while the index is loading, the
// recipe is read from the API. A recipe Paprika does not have is
// [paprikaclient.ErrNotFound].
func (m *Mirror) Recipe(ctx context.Context, uid string) (paprikaclient.Entry[paprikaclient.Recipe], error) {
	if m.Progress().Ready {
		if err := m.ensureRecipesFresh(ctx); err != nil {
			m.log.Warn("could not check for recipe changes; serving held data", "error", err)
		}
		if e, ok := m.IndexEntry(uid); ok {
			if body, hit := m.bodies.get(uid, e.Hash); hit {
				return body, nil
			}
		}
	}
	body, err := m.src.GetRecipe(ctx, uid)
	if err != nil {
		if errors.Is(err, paprikaclient.ErrNotFound) {
			m.forget(uid)
		}
		return body, err
	}
	m.UpsertRecipe(body)
	return body, nil
}

// UpsertRecipe stores a recipe as read or written by the caller: it updates the
// index entry and the body cache. The write path calls it after a successful
// save, with the object it sent.
func (m *Mirror) UpsertRecipe(body paprikaclient.Entry[paprikaclient.Recipe]) {
	if body.Value.UID == "" {
		return
	}
	m.bodies.put(body)
	m.storeEntry(newIndexEntry(body.Value, ""))
}

func (m *Mirror) forget(uid string) {
	m.bodies.remove(uid)
	m.mu.Lock()
	_, had := m.index[uid]
	delete(m.index, uid)
	m.mu.Unlock()
	if had {
		m.changed()
	}
}

// IndexEntry returns the index entry for uid. The entry is shared and must not
// be modified.
func (m *Mirror) IndexEntry(uid string) (*IndexEntry, bool) {
	m.mu.RLock()
	defer m.mu.RUnlock()
	e, ok := m.index[uid]
	return e, ok
}

// RecipeName returns the indexed name of a recipe, or "" when it is unknown.
func (m *Mirror) RecipeName(uid string) string {
	if e, ok := m.IndexEntry(uid); ok {
		return e.Name
	}
	return ""
}

// storeEntry puts e in the index, sharing repeated strings, and reports a change.
func (m *Mirror) storeEntry(e *IndexEntry) {
	m.mu.Lock()
	e.internWith(m.intern)
	old, had := m.index[e.UID]
	m.index[e.UID] = e
	m.mu.Unlock()
	if !had || !old.equal(e) {
		m.changed()
	}
}

// expiredLocked reports whether recipes are due for a full re-read. The caller
// holds mu.
func (m *Mirror) expiredLocked() bool {
	return !m.fullReadAt.IsZero() && m.now().Sub(m.fullReadAt) > m.expiry
}
