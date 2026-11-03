package cache_test

import (
	"bytes"
	"compress/gzip"
	"context"
	"fmt"
	"log/slog"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/cache"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient"
)

// syntheticSource serves a generated library of n recipes from memory.
type syntheticSource struct {
	paprikaclient.Client // nil; only the methods below are used
	n                    int
}

var vocabulary = strings.Fields(`flour sugar butter eggs milk cream salt pepper garlic onion tomato basil oregano thyme
	rosemary chicken beef pork lamb salmon shrimp rice pasta noodles potato carrot celery spinach kale lettuce cucumber
	pepper mushroom cheese yogurt lemon lime orange apple banana cinnamon vanilla chocolate almonds walnuts honey oil
	vinegar mustard ketchup soy ginger cumin paprika turmeric coriander parsley cilantro chili beans lentils chickpeas
	broccoli cauliflower zucchini eggplant squash pumpkin corn peas avocado coconut mango pineapple`)

func title(w string) string { return strings.ToUpper(w[:1]) + w[1:] }

func syntheticRecipe(i int) paprikaclient.Recipe {
	var ing strings.Builder
	for j := range 12 {
		w1 := vocabulary[(i*7+j*13)%len(vocabulary)]
		w2 := vocabulary[(i*11+j*17+3)%len(vocabulary)]
		fmt.Fprintf(&ing, "%d cups %s %s, finely chopped\n", j+1, w1, w2)
	}
	return paprikaclient.Recipe{
		UID:         fmt.Sprintf("%08X-0000-4000-8000-%012X", i, i),
		Hash:        fmt.Sprintf("%064X", i*2654435761),
		Name:        fmt.Sprintf("%s and %s Casserole Number %d", title(vocabulary[i%len(vocabulary)]), title(vocabulary[(i*3)%len(vocabulary)]), i),
		Created:     "2030-01-01 00:00:00",
		Ingredients: ing.String(),
		Directions:  strings.Repeat("Mix well and bake until golden. ", 40),
		Source:      fmt.Sprintf("Cookbook %d", i%40),
		Rating:      i % 6,
		OnFavorites: i%9 == 0,
		TotalTime:   "45 min",
		Categories: []string{
			fmt.Sprintf("CAT-%d", i%12),
			fmt.Sprintf("CAT-%d", 12+i%5),
		},
	}
}

func (s *syntheticSource) Status(context.Context) (paprikaclient.Counters, error) {
	return paprikaclient.Counters{"recipes": 1}, nil
}

func (s *syntheticSource) ListRecipes(context.Context) ([]paprikaclient.RecipeRef, error) {
	refs := make([]paprikaclient.RecipeRef, s.n)
	for i := range refs {
		r := syntheticRecipe(i)
		refs[i] = paprikaclient.RecipeRef{UID: r.UID, Hash: r.Hash}
	}
	return refs, nil
}

func (s *syntheticSource) GetRecipe(_ context.Context, uid string) (paprikaclient.Entry[paprikaclient.Recipe], error) {
	var i int
	if _, err := fmt.Sscanf(uid[:8], "%X", &i); err != nil {
		return paprikaclient.Entry[paprikaclient.Recipe]{}, paprikaclient.ErrNotFound
	}
	return paprikaclient.Entry[paprikaclient.Recipe]{Value: syntheticRecipe(i)}, nil
}

func (s *syntheticSource) Categories(context.Context) ([]paprikaclient.Entry[paprikaclient.Category], error) {
	var out []paprikaclient.Entry[paprikaclient.Category]
	for i := range 17 {
		out = append(out, paprikaclient.Entry[paprikaclient.Category]{Value: paprikaclient.Category{UID: fmt.Sprintf("CAT-%d", i), Name: fmt.Sprintf("Category %d", i)}})
	}
	return out, nil
}

// TestMemoryAndSpeedAtFiveThousandRecipes checks that a large library fits the
// service's 128 MiB limit with plenty of room, and that search stays quick. The
// numbers are logged for the record.
func TestMemoryAndSpeedAtFiveThousandRecipes(t *testing.T) {
	if testing.Short() {
		t.Skip("scale test")
	}
	const n = 5000
	ctx := context.Background()
	src := &syntheticSource{n: n}

	runtime.GC()
	var before runtime.MemStats
	runtime.ReadMemStats(&before)

	m := cache.New(src, cache.Options{Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil))})
	if err := m.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	runtime.GC()
	var after runtime.MemStats
	runtime.ReadMemStats(&after)
	heap := int64(after.HeapAlloc) - int64(before.HeapAlloc)

	snap, err := m.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var gz bytes.Buffer
	zw := gzip.NewWriter(&gz)
	_, _ = zw.Write(snap.Payload)
	_ = zw.Close()

	start := time.Now()
	hits, err := m.Search(ctx, cache.Query{Text: "chicken rice"})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("5000-recipe index: heap %.1f MiB (%d bytes per recipe), snapshot %.1f MiB JSON / %.1f MiB gzip, search %v (%d hits)",
		float64(heap)/(1<<20), heap/n, float64(len(snap.Payload))/(1<<20), float64(gz.Len())/(1<<20), elapsed, len(hits))

	if got := m.Progress().Recipes; got != n {
		t.Fatalf("indexed %d recipes, want %d", got, n)
	}
	if heap > 40<<20 {
		t.Errorf("index uses %d MiB, want well under the 128 MiB pod limit", heap>>20)
	}
	if elapsed > time.Second {
		t.Errorf("search took %v", elapsed)
	}
}
