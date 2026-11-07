package caching_test

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"maps"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/caching"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/testsupport"
)

const (
	day    = 24 * time.Hour
	expiry = caching.DefaultExpiry
	// maxJitter is the most a stamp can be moved earlier: a tenth of the expiry.
	maxJitter = expiry / 10
)

// recipeReads counts the GetRecipe requests per uid that reached the fake, and
// lists the uids in the order the requests arrived.
func recipeReads(e *env) (map[string]int, []string) {
	counts := map[string]int{}
	var order []string
	for _, r := range e.fake.Requests() {
		if r.Method != "GET" || !strings.HasPrefix(r.Path, recipePathPrefix) {
			continue
		}
		uid := strings.Trim(strings.TrimPrefix(r.Path, recipePathPrefix), "/")
		counts[uid]++
		order = append(order, uid)
	}
	return counts, order
}

// flakySource fails every failEvery-th GetRecipe call, as a stalled Paprika
// would. Failed calls never reach the fake, so its request log shows only the
// reads that went through.
type flakySource struct {
	caching.Source
	failEvery atomic.Int64
	calls     atomic.Int64
}

func (f *flakySource) GetRecipe(
	ctx context.Context,
	uid string,
) (paprikaclient.Entry[paprikaclient.Recipe], error) {
	if n := f.failEvery.Load(); n > 0 && f.calls.Add(1)%n == 0 {
		return paprikaclient.Entry[paprikaclient.Recipe]{}, errors.New("stalled")
	}
	return f.Source.GetRecipe(ctx, uid)
}

func (e *env) newFlakyCache() (*caching.Cache, *flakySource) {
	src := &flakySource{Source: e.src}
	c := caching.New(src, caching.Options{
		Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		Now:    e.clock.Now,
	})
	return c, src
}

func TestAgeBasedRereadOnlyForOldEntries(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name    string
		advance time.Duration
		want    map[string]int
	}{
		{
			name:    "nothing is old enough",
			advance: 5 * day,
			want:    map[string]int{},
		},
		{
			name:    "only the older entries are due",
			advance: 15 * day,
			want:    map[string]int{"R1": 1, "R2": 1, "R3": 1},
		},
		{
			name:    "everything is due",
			advance: 31 * day,
			want:    map[string]int{"R1": 1, "R2": 1, "R3": 1, "R4": 1},
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			seedThree(e)
			c := e.newCache(nil)
			if err := c.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			e.clock.Advance(20 * day)
			e.fake.SeedRecipe(testsupport.Recipe("R4", "Fresh Bread", "flour"))
			if err := c.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			e.fake.ResetRequests()

			e.clock.Advance(tt.advance)
			if err := c.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			got, _ := recipeReads(e)
			if !maps.Equal(got, tt.want) {
				t.Errorf("reads = %v, want %v", got, tt.want)
			}
			if err := c.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			if again, _ := recipeReads(e); !maps.Equal(again, tt.want) {
				t.Errorf("a second pass read more: %v", again)
			}
		})
	}
}

func TestInterruptedRereadResumesWithoutRepeating(t *testing.T) {
	const total = 12
	ctx := context.Background()
	e := newEnv(t)
	for i := 1; i <= total; i++ {
		uid := fmt.Sprintf("R%02d", i)
		e.fake.SeedRecipe(testsupport.Recipe(uid, "Recipe "+uid, "salt"))
	}
	c, src := e.newFlakyCache()
	if err := c.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	for period := 1; period <= 2; period++ {
		e.clock.Advance(31 * day)
		e.fake.ResetRequests()
		src.failEvery.Store(4)

		passes := 0
		for {
			passes++
			if passes > 20 {
				t.Fatalf("period %d: still failing after %d passes", period, passes)
			}
			if err := c.Sync(ctx); err == nil {
				break
			}
			if p := c.Progress(); !p.Ready || p.Indexed != total {
				t.Fatalf("period %d: progress = %+v, want ready with %d", period, p, total)
			}
		}
		if passes < 4 {
			t.Errorf("period %d: finished in %d passes, want stalls to force more", period, passes)
		}
		reads, _ := recipeReads(e)
		if len(reads) != total {
			t.Errorf("period %d: read %d distinct recipes, want %d", period, len(reads), total)
		}
		for uid, n := range reads {
			if n != 1 {
				t.Errorf("period %d: %s read %d times, want once", period, uid, n)
			}
		}

		e.fake.ResetRequests()
		src.failEvery.Store(0)
		if err := c.Sync(ctx); err != nil {
			t.Fatal(err)
		}
		if n := e.recipeFetches(); n != 0 {
			t.Errorf("period %d: a pass after the re-read fetched %d recipes", period, n)
		}
	}
}

func TestReadinessReadsComeBeforeAgeBasedReads(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	seedThree(e)
	c := e.newCache(nil)
	if err := c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	readAt := map[string]time.Time{}
	for _, uid := range []string{"R1", "R3"} {
		entry, _ := c.IndexEntry(uid)
		readAt[uid] = entry.ReadAt
	}

	e.clock.Advance(31 * day)
	e.fake.SeedRecipe(testsupport.Recipe("R2", "Apple Crumble", "apples\noats", "CAT-DESSERT"))
	e.fake.SeedRecipe(testsupport.Recipe("R4", "Fresh Bread", "flour"))
	e.fake.ResetRequests()
	if err := c.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	_, order := recipeReads(e)
	if len(order) != 4 {
		t.Fatalf("read order = %v, want 4 reads", order)
	}
	first := slices.Clone(order[:2])
	slices.Sort(first)
	if !slices.Equal(first, []string{"R2", "R4"}) {
		t.Errorf("first reads = %v, want the changed R2 and the new R4", order[:2])
	}
	wantAged := []string{"R1", "R3"}
	slices.SortFunc(wantAged, func(a, b string) int { return readAt[a].Compare(readAt[b]) })
	if !slices.Equal(order[2:], wantAged) {
		t.Errorf("age-based reads = %v, want oldest first: %v", order[2:], wantAged)
	}
}

func TestReadyCacheStaysReadyWhileAgeBasedRereadsFail(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name         string
		fromSnapshot bool
	}{
		{name: "index built in this process"},
		{name: "index loaded from a snapshot", fromSnapshot: true},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			seedThree(e)
			store, _ := newFileStore(t)
			c := e.newCache(store)
			if err := c.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			e.clock.Advance(31 * day)
			if tt.fromSnapshot {
				saveSnapshot(t, c, store)
				c = e.newCache(store)
				c.LoadSnapshot(ctx)
			}
			for _, uid := range []string{"R1", "R2", "R3"} {
				failRecipe(e, uid, 3)
			}

			if p := c.Progress(); !p.Ready {
				t.Fatalf("before the pass: %+v, want ready", p)
			}
			if err := c.Sync(ctx); err == nil {
				t.Fatal("expected the age-based re-read to fail")
			}
			if p := c.Progress(); !p.Ready || p.Indexed != 3 || p.Total != 3 {
				t.Errorf("progress = %+v, want ready with 3 of 3", p)
			}
			if _, err := c.Search(ctx, caching.Query{}); err != nil {
				t.Errorf("search while re-reads fail: %v", err)
			}
		})
	}
}

func TestJitterIsDeterministicAndBounded(t *testing.T) {
	const total = 40
	ctx := context.Background()
	e := newEnv(t)
	for i := 1; i <= total; i++ {
		uid := fmt.Sprintf("R%02d", i)
		e.fake.SeedRecipe(testsupport.Recipe(uid, "Recipe "+uid, "salt"))
	}
	now := e.clock.Now()
	first := e.newCache(nil)
	second := e.newCache(nil)
	for _, c := range []*caching.Cache{first, second} {
		if err := c.Sync(ctx); err != nil {
			t.Fatal(err)
		}
	}

	distinct := map[time.Time]struct{}{}
	for i := 1; i <= total; i++ {
		uid := fmt.Sprintf("R%02d", i)
		a, _ := first.IndexEntry(uid)
		b, _ := second.IndexEntry(uid)
		if !a.ReadAt.Equal(b.ReadAt) {
			t.Errorf("%s: stamps differ between caches: %v and %v", uid, a.ReadAt, b.ReadAt)
		}
		early := now.Sub(a.ReadAt)
		if early < 0 || early >= maxJitter {
			t.Errorf("%s: stamp is %v before the read, want [0, %v)", uid, early, maxJitter)
		}
		distinct[a.ReadAt] = struct{}{}
	}
	if len(distinct) < total/2 {
		t.Errorf("only %d distinct stamps for %d recipes; no spread", len(distinct), total)
	}
}

func TestChangedAndWrittenRecipesAreStampedWithTheReadTime(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	seedThree(e)
	c := e.newCache(nil)
	if err := c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(2 * day)
	e.fake.SeedRecipe(testsupport.Recipe("R2", "Apple Crumble", "apples\noats", "CAT-DESSERT"))
	if err := c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if entry, _ := c.IndexEntry("R2"); !entry.ReadAt.Equal(e.clock.Now()) {
		t.Errorf("changed recipe ReadAt = %v, want %v", entry.ReadAt, e.clock.Now())
	}

	e.clock.Advance(day)
	body, err := e.src.GetRecipe(ctx, "R3")
	if err != nil {
		t.Fatal(err)
	}
	c.UpsertRecipe(body)
	if entry, _ := c.IndexEntry("R3"); !entry.ReadAt.Equal(e.clock.Now()) {
		t.Errorf("upserted recipe ReadAt = %v, want %v", entry.ReadAt, e.clock.Now())
	}
}

func TestFreshRunsPassForAgedEntryWithoutCounterChange(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name        string
		advance     time.Duration
		wantList    int
		wantFetches int
	}{
		{
			name:    "young entries and a still counter",
			advance: 5 * day,
		},
		{
			name:        "aged entries and a still counter",
			advance:     31 * day,
			wantList:    1,
			wantFetches: 3,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			seedThree(e)
			c := e.newCache(nil)
			if err := c.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			e.clock.Advance(tt.advance)
			e.fake.ResetRequests()

			if err := c.Fresh(ctx); err != nil {
				t.Fatal(err)
			}
			if got := e.count("GET", recipesPath); got != tt.wantList {
				t.Errorf("recipe list reads = %d, want %d", got, tt.wantList)
			}
			if got := e.recipeFetches(); got != tt.wantFetches {
				t.Errorf("recipe fetches = %d, want %d", got, tt.wantFetches)
			}
			if err := c.Fresh(ctx); err != nil {
				t.Fatal(err)
			}
			if got := e.count("GET", recipesPath); got != tt.wantList {
				t.Errorf("recipe list reads after a second check = %d, want %d", got, tt.wantList)
			}
		})
	}
}

func TestSnapshotRoundTripsReadAt(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	seedThree(e)
	store, _ := newFileStore(t)
	first := e.newCache(store)
	if err := first.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	saveSnapshot(t, first, store)

	second := e.newCache(store)
	second.LoadSnapshot(ctx)
	for _, uid := range []string{"R1", "R2", "R3"} {
		want, _ := first.IndexEntry(uid)
		got, ok := second.IndexEntry(uid)
		if !ok {
			t.Fatalf("%s missing after the snapshot load", uid)
		}
		if want.ReadAt.IsZero() || !got.ReadAt.Equal(want.ReadAt) {
			t.Errorf("%s: ReadAt = %v, want %v", uid, got.ReadAt, want.ReadAt)
		}
	}
}

func TestAgeOnlyPassesAreLimitedToOnePerFreshWindow(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	seedThree(e)
	c, src := e.newFlakyCache()
	if err := c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(31 * day)
	src.failEvery.Store(1)
	e.fake.ResetRequests()

	for i := 0; i < 5; i++ {
		_ = c.Fresh(ctx)
		e.clock.Advance(10 * time.Second)
	}
	if got := e.count("GET", recipesPath); got != 1 {
		t.Errorf("recipe list reads within the window = %d, want 1", got)
	}
	if got := src.calls.Load(); got != 1 {
		t.Errorf("recipe read attempts within the window = %d, want 1", got)
	}

	e.clock.Advance(30 * time.Second)
	_ = c.Fresh(ctx)
	if got := e.count("GET", recipesPath); got != 2 {
		t.Errorf("recipe list reads after the window = %d, want 2", got)
	}
	if got := src.calls.Load(); got != 2 {
		t.Errorf("recipe read attempts after the window = %d, want 2", got)
	}
}

func TestMovedCounterStillStartsPassInsideFreshWindow(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	seedThree(e)
	c := e.newCache(nil)
	if err := c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	e.clock.Advance(61 * time.Second)
	e.fake.SeedRecipe(testsupport.Recipe("R4", "Fresh Bread", "flour"))
	if err := c.Fresh(ctx); err != nil {
		t.Fatal(err)
	}
	if _, ok := c.IndexEntry("R4"); !ok {
		t.Error("a moved counter did not start a pass")
	}
}
