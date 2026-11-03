package caching_test

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/caching"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/indexstore"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient/fakepaprika"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/testsupport"
)

const (
	recipePathPrefix = "/api/v2/sync/recipe/"
	recipesPath      = "/api/v2/sync/recipes/"
	statusPath       = "/api/v2/sync/status/"
	categoriesPath   = "/api/v2/sync/categories/"
	mealsPath        = "/api/v2/sync/meals/"
)

// clock is a settable time source.
type clock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *clock { return &clock{t: time.Date(2030, 1, 1, 12, 0, 0, 0, time.UTC)} }

func (c *clock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *clock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

type env struct {
	fake  *fakepaprika.Server
	src   *paprikaclient.Client
	clock *clock
	logs  *bytes.Buffer
}

func newEnv(t *testing.T) *env {
	t.Helper()
	fake := fakepaprika.New(t)
	return &env{
		fake:  fake,
		src:   testsupport.Client(t, fake, nil),
		clock: newClock(),
		logs:  &bytes.Buffer{},
	}
}

func (e *env) newCache(store indexstore.Store) *caching.Cache {
	return caching.New(e.src, caching.Options{
		Logger:   slog.New(slog.NewTextHandler(&lockedWriter{w: e.logs}, nil)),
		Store:    store,
		Now:      e.clock.Now,
		RetryMin: time.Millisecond,
		RetryMax: 5 * time.Millisecond,
	})
}

type lockedWriter struct {
	mu sync.Mutex
	w  *bytes.Buffer
}

func (l *lockedWriter) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.w.Write(p)
}

func (e *env) count(method, path string) int {
	return len(e.fake.RequestsTo(method, path))
}

func (e *env) recipeFetches() int {
	n := 0
	for _, r := range e.fake.Requests() {
		if r.Method == "GET" && strings.HasPrefix(r.Path, recipePathPrefix) {
			n++
		}
	}
	return n
}

func seedThree(e *env) {
	e.fake.Seed(
		"categories",
		fakepaprika.SampleCategory("CAT-SOUP", "Soups"),
		fakepaprika.SampleCategory("CAT-DESSERT", "Desserts"),
	)
	e.fake.SeedRecipe(testsupport.Recipe(
		"R1",
		"Chicken Soup",
		"chicken\ncarrots\nonion",
		"CAT-SOUP",
	))
	e.fake.SeedRecipe(testsupport.Recipe("R2", "Apple Pie", "apples\nsugar\nflour", "CAT-DESSERT"))
	e.fake.SeedRecipe(testsupport.Recipe("R3", "Roast Dinner", "chicken\npotatoes"))
}

func TestRefreshOnlyOnCounterChange(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	seedThree(e)
	c := e.newCache(nil)

	if _, err := c.Categories(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Categories(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.count("GET", statusPath); got != 1 {
		t.Errorf("status calls within the fresh window = %d, want 1", got)
	}
	if got := e.count("GET", categoriesPath); got != 1 {
		t.Errorf("category downloads = %d, want 1", got)
	}

	steps := []struct {
		name           string
		advance        time.Duration
		change         func()
		wantStatus     int
		wantCategories int
	}{
		{"inside the window nothing is checked", 30 * time.Second, nil, 1, 1},
		{"past the window with no change checks status only", 31 * time.Second, nil, 2, 1},
		{
			"a changed counter reloads categories",
			61 * time.Second,
			func() {
				e.fake.Seed("categories", fakepaprika.SampleCategory("CAT-NEW", "Salads"))
			},
			3,
			2,
		},
		{
			"a change to another resource does not reload categories",
			61 * time.Second,
			func() {
				e.fake.Seed("meals", fakepaprika.SampleMeal("M1", "x", "2030-01-02 00:00:00", "T"))
			},
			4,
			2,
		},
	}
	for _, s := range steps {
		t.Run(s.name, func(t *testing.T) {
			e.clock.Advance(s.advance)
			if s.change != nil {
				s.change()
			}
			cats, err := c.Categories(ctx)
			if err != nil {
				t.Fatal(err)
			}
			if got := e.count("GET", statusPath); got != s.wantStatus {
				t.Errorf("status calls = %d, want %d", got, s.wantStatus)
			}
			if got := e.count("GET", categoriesPath); got != s.wantCategories {
				t.Errorf("category downloads = %d, want %d", got, s.wantCategories)
			}
			if s.wantCategories == 2 && len(cats) != 3 {
				t.Errorf("categories = %d, want 3", len(cats))
			}
		})
	}
	if got := e.count("GET", mealsPath); got != 0 {
		t.Errorf("meals were downloaded %d times without being read", got)
	}
}

func TestInvalidateAndRefresh(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	seedThree(e)
	c := e.newCache(nil)
	if _, err := c.Categories(ctx); err != nil {
		t.Fatal(err)
	}
	if c.Invalidate("nonsense") {
		t.Error("Invalidate accepted an unknown resource")
	}
	if err := c.Refresh(ctx, "nonsense"); err == nil {
		t.Error("Refresh accepted an unknown resource")
	}
	if err := c.Refresh(ctx, caching.ResourceCategories); err != nil {
		t.Fatal(err)
	}
	if got := e.count("GET", categoriesPath); got != 2 {
		t.Errorf("category downloads after Refresh = %d, want 2", got)
	}
}

func TestHashDiffing(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	seedThree(e)
	c := e.newCache(nil)

	if err := c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.recipeFetches(); got != 3 {
		t.Fatalf("first pass fetched %d recipes, want 3", got)
	}
	if err := c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.recipeFetches(); got != 3 {
		t.Errorf("a pass with no changes fetched %d more recipes", got-3)
	}

	e.fake.SeedRecipe(testsupport.Recipe(
		"R1",
		"Chicken Noodle Soup",
		"chicken\nnoodles",
		"CAT-SOUP",
	))
	e.fake.SeedRecipe(testsupport.Recipe("R4", "Brand New", "kale"))
	e.fake.SeedRecipe(map[string]any{"uid": "R2", "deleted": true})
	if err := c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.recipeFetches(); got != 5 {
		t.Errorf("fetches after changes = %d, want 5 (only the changed and the new recipe)", got)
	}
	if _, ok := c.IndexEntry("R2"); ok {
		t.Error("removed recipe is still indexed")
	}
	if got := c.RecipeName("R1"); got != "Chicken Noodle Soup" {
		t.Errorf("changed recipe name = %q", got)
	}
	if got := c.RecipeName("R4"); got != "Brand New" {
		t.Errorf("new recipe name = %q", got)
	}
	if p := c.Progress(); !p.Ready || p.Recipes != 3 {
		t.Errorf("progress = %+v, want ready with 3 recipes", p)
	}
}

func TestBodyCache(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	seedThree(e)
	c := e.newCache(nil)
	if err := c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	base := e.recipeFetches()

	for i := range 3 {
		r, err := c.Recipe(ctx, "R1")
		if err != nil {
			t.Fatal(err)
		}
		if r.Value.Name != "Chicken Soup" || r.Raw["future_field"] == nil {
			t.Fatalf("read %d returned %q without its raw fields", i, r.Value.Name)
		}
	}
	if got := e.recipeFetches() - base; got != 1 {
		t.Errorf("three reads cost %d body fetches, want 1", got)
	}

	// The recipe changes elsewhere: the index catches up, the old body must not be served.
	e.fake.SeedRecipe(testsupport.Recipe(
		"R1",
		"Chicken Noodle Soup",
		"chicken\nnoodles",
		"CAT-SOUP",
	))
	e.clock.Advance(2 * time.Minute)
	r, err := c.Recipe(ctx, "R1")
	if err != nil {
		t.Fatal(err)
	}
	if r.Value.Name != "Chicken Noodle Soup" {
		t.Errorf("served stale body %q", r.Value.Name)
	}

	// A body whose hash differs from the index is never served, even with a fresh status.
	if _, err := c.Recipe(ctx, "NOPE"); !errors.Is(err, paprikaclient.ErrNotFound) {
		t.Errorf("unknown recipe error = %v, want ErrNotFound", err)
	}
}

func TestBodyCacheEvictsLeastRecentlyUsed(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	seedThree(e)
	c := caching.New(e.src, caching.Options{
		Now:           e.clock.Now,
		BodyCacheSize: 2,
		Logger:        slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
	})
	if err := c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	for _, uid := range []string{"R1", "R2", "R3"} {
		if _, err := c.Recipe(ctx, uid); err != nil {
			t.Fatal(err)
		}
	}
	before := e.recipeFetches()
	if _, err := c.Recipe(ctx, "R3"); err != nil { // cached
		t.Fatal(err)
	}
	if _, err := c.Recipe(ctx, "R1"); err != nil { // evicted
		t.Fatal(err)
	}
	if got := e.recipeFetches() - before; got != 1 {
		t.Errorf("fetches = %d, want 1 (R3 cached, R1 evicted)", got)
	}
}

func newFileStore(t *testing.T) (indexstore.Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "index.json.gz")
	store, err := indexstore.New(context.Background(), indexstore.Config{
		Mode:          indexstore.ModeFilesystem,
		FilePath:      path,
		FormatVersion: caching.SnapshotFormatVersion,
	})
	if err != nil {
		t.Fatal(err)
	}
	return store, path
}

func saveSnapshot(t *testing.T, c *caching.Cache, store indexstore.Store) {
	t.Helper()
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Save(context.Background(), snap); err != nil {
		t.Fatal(err)
	}
}

func TestSnapshotLoadCompareThenBackgroundFetch(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	seedThree(e)
	store, _ := newFileStore(t)

	first := e.newCache(store)
	if err := first.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	saveSnapshot(t, first, store)

	// While the service was down: one recipe changed and one was added.
	e.fake.SeedRecipe(testsupport.Recipe("R2", "Apple Crumble", "apples\noats", "CAT-DESSERT"))
	e.fake.SeedRecipe(testsupport.Recipe("R5", "Lentil Stew", "lentils"))
	e.fake.ResetRequests()

	second := e.newCache(store)
	second.LoadSnapshot(ctx)
	if p := second.Progress(); !p.Ready || p.Recipes != 3 {
		t.Fatalf("after loading the snapshot progress = %+v, want ready with 3 recipes", p)
	}
	hits, err := second.Search(ctx, caching.Query{Text: "chicken"})
	if err != nil || len(hits) != 2 {
		t.Fatalf("search from snapshot = %d hits, %v; want 2", len(hits), err)
	}
	if got := e.recipeFetches(); got != 0 {
		t.Fatalf("loading the snapshot fetched %d recipes", got)
	}

	if err := second.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.count("GET", recipesPath); got != 1 {
		t.Errorf("recipe list reads = %d, want 1", got)
	}
	if got := e.recipeFetches(); got != 2 {
		t.Errorf("fetches = %d, want 2 (one changed, one new)", got)
	}
	if got := second.RecipeName("R2"); got != "Apple Crumble" {
		t.Errorf("changed recipe name = %q", got)
	}
}

func TestRunLoadsSnapshotThenSyncs(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := newEnv(t)
	seedThree(e)
	store, _ := newFileStore(t)
	first := e.newCache(store)
	if err := first.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	saveSnapshot(t, first, store)
	e.fake.SeedRecipe(testsupport.Recipe("R6", "Fresh Bread", "flour"))

	second := e.newCache(store)
	done := make(chan struct{})
	go func() { second.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("Run did not finish")
	}
	if _, ok := second.IndexEntry("R6"); !ok {
		t.Error("Run did not fetch the new recipe")
	}
}

func TestSnapshotExpiryRereadsEverythingWhileServing(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	seedThree(e)
	store, _ := newFileStore(t)
	first := e.newCache(store)
	if err := first.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	saveSnapshot(t, first, store)
	e.fake.ResetRequests()

	e.clock.Advance(31 * 24 * time.Hour)
	second := e.newCache(store)
	second.LoadSnapshot(ctx)
	if !second.Progress().Ready {
		t.Fatal("an old snapshot must still serve searches")
	}
	if _, err := second.Search(ctx, caching.Query{Text: "apple"}); err != nil {
		t.Fatal(err)
	}
	if err := second.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.recipeFetches(); got != 3 {
		t.Errorf("expired snapshot: fetches = %d, want all 3 recipes", got)
	}
	if err := second.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.recipeFetches(); got != 3 {
		t.Errorf("after the full re-read another pass fetched %d recipes", got-3)
	}
}

func TestFreshSnapshotIsNotReread(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	seedThree(e)
	store, _ := newFileStore(t)
	first := e.newCache(store)
	if err := first.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	saveSnapshot(t, first, store)
	e.fake.ResetRequests()

	e.clock.Advance(29 * 24 * time.Hour)
	second := e.newCache(store)
	second.LoadSnapshot(ctx)
	if err := second.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.recipeFetches(); got != 0 {
		t.Errorf("a 29 day old snapshot caused %d fetches", got)
	}
}

func TestUnusableSnapshotIsIgnored(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name  string
		write func(t *testing.T, path string)
	}{
		{"garbage", func(t *testing.T, path string) { writeFile(t, path, []byte("not gzip")) }},
		{
			"other format version",
			func(t *testing.T, path string) {
				other, err := indexstore.New(
					ctx,
					indexstore.Config{
						Mode:          indexstore.ModeFilesystem,
						FilePath:      path,
						FormatVersion: 99,
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				if err := other.Save(
					ctx,
					indexstore.Snapshot{Payload: []byte(`{"complete":true,"recipes":[]}`)},
				); err != nil {
					t.Fatal(err)
				}
			},
		},
		{
			"payload of another shape",
			func(t *testing.T, path string) {
				other, err := indexstore.New(
					ctx,
					indexstore.Config{
						Mode:          indexstore.ModeFilesystem,
						FilePath:      path,
						FormatVersion: caching.SnapshotFormatVersion,
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				if err := other.Save(
					ctx,
					indexstore.Snapshot{Payload: []byte(`["nope"]`)},
				); err != nil {
					t.Fatal(err)
				}
			},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			seedThree(e)
			store, path := newFileStore(t)
			tt.write(t, path)
			c := e.newCache(store)
			c.LoadSnapshot(ctx)
			if c.Progress().Ready || c.Progress().Recipes != 0 {
				t.Errorf("unusable snapshot was used: %+v", c.Progress())
			}
			if !strings.Contains(e.logs.String(), "unusable") {
				t.Errorf("no warning logged: %s", e.logs.String())
			}
			if err := c.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			if got := e.recipeFetches(); got != 3 {
				t.Errorf("full load fetched %d recipes, want 3", got)
			}
		})
	}
}

func TestPartialSnapshotResumesButDoesNotServe(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	seedThree(e)
	store, _ := newFileStore(t)

	gate := newGatedSource(e.src, "R2")
	first := caching.New(
		gate,
		caching.Options{
			Now:    e.clock.Now,
			Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		},
	)
	errc := make(chan error, 1)
	go func() { errc <- first.Sync(ctx) }()
	gate.waitBlocked(t)
	saveSnapshot(t, first, store) // one recipe indexed, pass not finished
	gate.fail(errors.New("stop"))
	if err := <-errc; err == nil {
		t.Fatal("expected the interrupted pass to fail")
	}

	e.fake.ResetRequests()
	second := e.newCache(store)
	second.LoadSnapshot(ctx)
	if second.Progress().Ready {
		t.Error("a partial snapshot must not make search available")
	}
	if _, err := second.Search(ctx, caching.Query{}); err == nil {
		t.Error("search succeeded on a partial snapshot")
	}
	if err := second.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if got := e.recipeFetches(); got != 2 {
		t.Errorf("resumed pass fetched %d recipes, want 2; logs: %s", got, e.logs.String())
	}
	if !second.Progress().Ready {
		t.Error("not ready after the pass")
	}
}

// gatedSource holds GetRecipe for one uid until the test lets it go.
type gatedSource struct {
	*paprikaclient.Client
	blockUID string
	blocked  chan struct{}
	release  chan error
	once     sync.Once
}

func newGatedSource(c *paprikaclient.Client, blockUID string) *gatedSource {
	return &gatedSource{
		Client:   c,
		blockUID: blockUID,
		blocked:  make(chan struct{}),
		release:  make(chan error, 1),
	}
}

func (g *gatedSource) GetRecipe(
	ctx context.Context,
	uid string,
) (paprikaclient.Entry[paprikaclient.Recipe], error) {
	if uid == g.blockUID {
		g.once.Do(func() { close(g.blocked) })
		select {
		case err := <-g.release:
			if err != nil {
				return paprikaclient.Entry[paprikaclient.Recipe]{}, err
			}
		case <-ctx.Done():
			return paprikaclient.Entry[paprikaclient.Recipe]{}, ctx.Err()
		}
	}
	return g.Client.GetRecipe(ctx, uid)
}

func (g *gatedSource) waitBlocked(t *testing.T) {
	t.Helper()
	select {
	case <-g.blocked:
	case <-time.After(10 * time.Second):
		t.Fatal("source was never blocked")
	}
}

func (g *gatedSource) fail(err error) { g.release <- err }
func (g *gatedSource) open()          { g.release <- nil }

func TestStillLoadingUntilFirstPassFinishes(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := newEnv(t)
	seedThree(e)
	gate := newGatedSource(e.src, "R2")
	c := caching.New(
		gate,
		caching.Options{
			Now:    e.clock.Now,
			Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		},
	)

	done := make(chan struct{})
	go func() { _ = c.Sync(ctx); close(done) }()
	gate.waitBlocked(t)

	_, err := c.Search(ctx, caching.Query{Text: "soup"})
	var loading *caching.LoadingError
	if !errors.As(err, &loading) {
		t.Fatalf("search error = %v, want *LoadingError", err)
	}
	if loading.Total != 3 || loading.Done != 1 {
		t.Errorf("progress = %d of %d, want 1 of 3", loading.Done, loading.Total)
	}
	if !strings.Contains(loading.Error(), "1 of 3") {
		t.Errorf("message %q lacks progress", loading.Error())
	}

	// get_recipe still works against the API meanwhile.
	r, err := c.Recipe(ctx, "R3")
	if err != nil || r.Value.Name != "Roast Dinner" {
		t.Fatalf("Recipe while loading = %q, %v", r.Value.Name, err)
	}

	gate.open()
	<-done
	if _, err := c.Search(ctx, caching.Query{Text: "soup"}); err != nil {
		t.Errorf("search after loading: %v", err)
	}
}

func TestRunRetriesAfterTemporaryFailure(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := newEnv(t)
	seedThree(e)
	e.fake.Inject(fakepaprika.Fault{
		Method:  "GET",
		Path:    recipesPath,
		Times:   2,
		Kind:    fakepaprika.FaultStatus,
		Status:  500,
		Message: "boom",
	})
	c := e.newCache(nil)
	done := make(chan struct{})
	go func() { c.Run(ctx); close(done) }()
	select {
	case <-done:
	case <-time.After(20 * time.Second):
		t.Fatal("Run never succeeded")
	}
	if !c.Progress().Ready {
		t.Error("not ready after retries")
	}
}

func TestSearch(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	e.fake.Seed(
		"categories",
		fakepaprika.SampleCategory("CAT-SOUP", "Soups"),
		fakepaprika.SampleCategory("CAT-DESSERT", "Desserts"),
	)
	add := func(
		uid, name, ingredients, source string,
		rating int,
		fav, trash bool,
		cats ...string,
	) {
		r := testsupport.Recipe(uid, name, ingredients, cats...)
		r["source"] = source
		r["rating"] = rating
		r["on_favorites"] = fav
		r["in_trash"] = trash
		e.fake.SeedRecipe(r)
	}
	add("A", "Tomato Soup", "tomatoes\ncream\nbasil", "Grandma", 5, true, false, "CAT-SOUP")
	add("B", "Chicken Stew", "chicken\ntomatoes\ncarrots", "Example Kitchen", 3, false, false)
	add("C", "Garden Salad", "lettuce\ntomatoes", "Example Kitchen", 0, false, false)
	add("D", "Old Soup", "water", "", 1, false, true, "CAT-SOUP")
	add("E", "Apple Pie", "apples\ncinnamon", "Grandma's Book", 4, true, false, "CAT-DESSERT")
	add("F", "Soup Dumplings", "pork\nflour", "", 2, false, false)
	c := e.newCache(nil)
	if err := c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	yes, no := true, false

	tests := []struct {
		name string
		q    caching.Query
		want []string
	}{
		{"name outranks ingredient", caching.Query{Text: "tomato"}, []string{"A", "B", "C"}},
		{"name match before category match", caching.Query{Text: "soup"}, []string{"F", "A"}},
		{"category name matches", caching.Query{Text: "desserts"}, []string{"E"}},
		{"source matches", caching.Query{Text: "grandma"}, []string{"E", "A"}},
		{"prefix of a word", caching.Query{Text: "carr"}, []string{"B"}},
		{"case insensitive", caching.Query{Text: "CHICKEN"}, []string{"B"}},
		{"all words must match", caching.Query{Text: "tomato salad"}, []string{"C"}},
		{"words in any field", caching.Query{Text: "soup cream"}, []string{"A"}},
		{"no match", caching.Query{Text: "zucchini"}, nil},
		{"directions are not searchable", caching.Query{Text: "boil"}, nil},
		{"trashed hidden by default", caching.Query{Text: "old"}, nil},
		{
			"trashed included on request",
			caching.Query{Text: "old", IncludeTrashed: true},
			[]string{"D"},
		},
		{"empty query lists by name", caching.Query{}, []string{"E", "B", "C", "F", "A"}},
		{"favorites only", caching.Query{Favorite: &yes}, []string{"E", "A"}},
		{"non favorites only", caching.Query{Favorite: &no}, []string{"B", "C", "F"}},
		{"minimum rating", caching.Query{MinRating: 4}, []string{"E", "A"}},
		{"category filter", caching.Query{CategoryUID: "CAT-SOUP"}, []string{"A"}},
		{
			"category filter with trashed",
			caching.Query{CategoryUID: "CAT-SOUP", IncludeTrashed: true},
			[]string{"D", "A"},
		},
		{
			"filters combine with text",
			caching.Query{Text: "tomato", MinRating: 4, Favorite: &yes},
			[]string{"A"},
		},
		{"punctuation is ignored", caching.Query{Text: "  tomato, soup! "}, []string{"A"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			hits, err := c.Search(ctx, tt.q)
			if err != nil {
				t.Fatal(err)
			}
			var got []string
			for _, h := range hits {
				got = append(got, h.UID)
			}
			if strings.Join(got, ",") != strings.Join(tt.want, ",") {
				t.Errorf("got %v, want %v", got, tt.want)
			}
		})
	}
}

func TestOnChangeFiresOnlyForRealChanges(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	seedThree(e)
	c := e.newCache(nil)
	var n atomic.Int32
	c.SetOnChange(func() { n.Add(1) })

	if err := c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	afterFirst := n.Load()
	if afterFirst == 0 {
		t.Fatal("first pass reported no change")
	}
	if err := c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if n.Load() != afterFirst {
		t.Error("a pass with no changes reported a change")
	}
	e.fake.SeedRecipe(testsupport.Recipe("R9", "Extra", "salt"))
	if err := c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if n.Load() == afterFirst {
		t.Error("a new recipe reported no change")
	}
}

func TestUpsertRecipeAfterWrite(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	seedThree(e)
	c := e.newCache(nil)
	if err := c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	fresh, err := e.src.GetRecipe(ctx, "R1")
	if err != nil {
		t.Fatal(err)
	}
	fresh.Value.Name = "Renamed Soup"
	fresh.Value.Hash = testsupport.Hash("renamed")
	c.UpsertRecipe(fresh)
	e.fake.ResetRequests()
	got, err := c.Recipe(ctx, "R1")
	if err != nil || got.Value.Name != "Renamed Soup" {
		t.Fatalf("Recipe = %q, %v", got.Value.Name, err)
	}
	if n := e.recipeFetches(); n != 0 {
		t.Errorf("a freshly written recipe cost %d fetches", n)
	}
	hits, _ := c.Search(ctx, caching.Query{Text: "renamed"})
	if len(hits) != 1 {
		t.Errorf("renamed recipe not searchable: %d hits", len(hits))
	}
}

func TestConcurrentUse(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	defer cancel()
	e := newEnv(t)
	seedThree(e)
	c := e.newCache(nil)
	if err := c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for j := range 20 {
				switch (i + j) % 6 {
				case 0:
					_, _ = c.Search(ctx, caching.Query{Text: "chicken"})
				case 1:
					_, _ = c.Recipe(ctx, "R1")
				case 2:
					_, _ = c.Categories(ctx)
				case 3:
					c.Invalidate(caching.ResourceCategories)
				case 4:
					e.clock.Advance(2 * time.Minute)
					_ = c.Fresh(ctx)
				case 5:
					_, _ = c.Snapshot(ctx)
					e.fake.SeedRecipe(testsupport.Recipe(
						"R3",
						"Roast Dinner",
						"chicken\npotatoes\n"+strings.Repeat("x", j),
					))
				}
			}
		}()
	}
	wg.Wait()
}

func writeFile(t *testing.T, path string, data []byte) {
	t.Helper()
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestLoadedIsLoggedOnceWithoutNames(t *testing.T) {
	e := newEnv(t)
	seedThree(e)
	c := e.newCache(nil)
	if err := c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err := c.Sync(context.Background()); err != nil {
		t.Fatal(err)
	}
	out := e.logs.String()
	if n := strings.Count(out, "recipe index loaded"); n != 1 {
		t.Fatalf("loaded lines = %d, want 1:\n%s", n, out)
	}
	if !strings.Contains(
		out,
		"recipes=3",
	) || !strings.Contains(out, "duration=") || !strings.Contains(out, "level=INFO") {
		t.Errorf("log = %s", out)
	}
	for _, name := range []string{"Chicken Soup", "Apple Pie", "Roast Dinner"} {
		if strings.Contains(out, name) {
			t.Errorf("log contains the recipe name %q", name)
		}
	}
}
