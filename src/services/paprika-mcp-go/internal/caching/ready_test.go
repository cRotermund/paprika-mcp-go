package caching_test

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/caching"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/indexstore"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient/fakepaprika"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/testsupport"
)

func recipePath(uid string) string { return recipePathPrefix + uid + "/" }

func failRecipe(e *env, uid string, times int) {
	e.fake.Inject(fakepaprika.Fault{
		Method:  "GET",
		Path:    recipePath(uid),
		Times:   times,
		Kind:    fakepaprika.FaultStatus,
		Status:  500,
		Message: "boom",
	})
}

func snapshotComplete(t *testing.T, c *caching.Cache) bool {
	t.Helper()
	snap, err := c.Snapshot(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	var p struct {
		Complete bool `json:"complete"`
	}
	if err := json.Unmarshal(snap.Payload, &p); err != nil {
		t.Fatal(err)
	}
	return p.Complete
}

func TestInterruptedPassesStillBecomeReady(t *testing.T) {
	const total = 12
	ctx := context.Background()
	e := newEnv(t)
	for i := 1; i <= total; i++ {
		uid := fmt.Sprintf("R%02d", i)
		e.fake.SeedRecipe(testsupport.Recipe(uid, "Recipe "+uid, "salt"))
		if i%4 == 0 {
			failRecipe(e, uid, 1)
		}
	}
	c := e.newCache(nil)

	last := 0
	passes := 0
	for !c.Progress().Ready {
		passes++
		if passes > 10 {
			t.Fatalf("not ready after %d passes: %+v", passes, c.Progress())
		}
		err := c.Sync(ctx)
		p := c.Progress()
		if p.Total != total {
			t.Errorf("pass %d: total = %d, want %d", passes, p.Total, total)
		}
		if p.Indexed < last {
			t.Errorf("pass %d: indexed went from %d down to %d", passes, last, p.Indexed)
		}
		if err != nil && p.Ready && p.Indexed != total {
			t.Errorf("pass %d: ready with %d of %d", passes, p.Indexed, total)
		}
		if err != nil && snapshotComplete(t, c) != p.Ready {
			t.Errorf("pass %d: snapshot complete flag disagrees with readiness", passes)
		}
		last = p.Indexed
	}
	if passes < 2 {
		t.Errorf("load took %d pass, want the faults to force several", passes)
	}
	if p := c.Progress(); p.Indexed != total || p.Recipes != total {
		t.Errorf("progress = %+v, want %d of %d", p, total, total)
	}
	if !snapshotComplete(t, c) {
		t.Error("snapshot of a ready cache is not complete")
	}
}

func TestReadyAsSoonAsLastMissingRecipeArrives(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e := newEnv(t)
	seedThree(e)
	store, _ := newFileStore(t)

	// Take a snapshot of a full index, then make it partial: R1 is missing, and
	// the full read is overdue, so the next pass reads R1 first and then reads
	// R2 and R3 again.
	full := e.newCache(nil)
	if err := full.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	snap, err := full.Snapshot(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(snap.Payload, &payload); err != nil {
		t.Fatal(err)
	}
	var entries []map[string]any
	if err := json.Unmarshal(payload["recipes"], &entries); err != nil {
		t.Fatal(err)
	}
	kept := entries[:0]
	for _, entry := range entries {
		if entry["u"] != "R1" {
			kept = append(kept, entry)
		}
	}
	payload["recipes"], _ = json.Marshal(kept)
	payload["complete"] = json.RawMessage("false")
	payload["full_read_at"], _ = json.Marshal(e.clock.Now().Add(-365 * 24 * time.Hour))
	raw, _ := json.Marshal(payload)
	if err := store.Save(ctx, indexstore.Snapshot{Payload: raw}); err != nil {
		t.Fatal(err)
	}

	gate := newGatedSource(e.src, "R3")
	c := caching.New(
		gate,
		caching.Options{
			Now:    e.clock.Now,
			Store:  store,
			Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		},
	)
	c.LoadSnapshot(ctx)
	if p := c.Progress(); p.Ready || p.Recipes != 2 {
		t.Fatalf("after load: %+v, want not ready with 2 recipes", p)
	}

	done := make(chan error, 1)
	go func() { done <- c.Sync(ctx) }()
	gate.waitBlocked(t)
	// R1 has been stored and the pass is still running, held on R3.
	if p := c.Progress(); !p.Ready || p.Indexed != 3 || p.Total != 3 {
		t.Errorf("mid-pass: %+v, want ready with 3 of 3", p)
	}
	if _, err := c.Search(ctx, caching.Query{Text: "soup"}); err != nil {
		t.Errorf("search mid-pass: %v", err)
	}
	gate.open()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if !c.Progress().Ready {
		t.Error("not ready after the pass")
	}
}

func TestChangedRecipeCountsAsNotYetIndexed(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	seedThree(e)

	c := e.newCache(nil)
	failRecipe(e, "R3", 1)
	if err := c.Sync(ctx); err == nil {
		t.Fatal("expected the interrupted pass to fail")
	}
	if p := c.Progress(); p.Ready || p.Indexed != 2 || p.Total != 3 {
		t.Fatalf("after the first pass: %+v, want 2 of 3", p)
	}

	// R1 changes in Paprika before the load finished: the stored copy is stale.
	failRecipe(e, "R3", 1)
	e.fake.SeedRecipe(testsupport.Recipe("R1", "Chicken Noodle Soup", "chicken\nnoodles"))
	if err := c.Sync(ctx); err == nil {
		t.Fatal("expected the second pass to fail")
	}
	if p := c.Progress(); p.Ready || p.Indexed != 2 || p.Total != 3 {
		t.Fatalf("after the change: %+v, want R2 and the new R1 only", p)
	}

	if err := c.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	if p := c.Progress(); !p.Ready || p.Indexed != 3 {
		t.Errorf("after the last pass: %+v, want ready with 3 of 3", p)
	}
	if got := c.RecipeName("R1"); got != "Chicken Noodle Soup" {
		t.Errorf("R1 name = %q", got)
	}
}

func TestReadyStaysReadyThroughFailingPasses(t *testing.T) {
	ctx := context.Background()
	for _, tt := range []struct {
		name        string
		fail        func(e *env)
		wantIndexed int
		wantTotal   int
	}{
		{
			name: "new recipe cannot be read",
			fail: func(e *env) {
				e.fake.SeedRecipe(testsupport.Recipe("R4", "Fresh Bread", "flour"))
				failRecipe(e, "R4", 1)
			},
			wantIndexed: 3,
			wantTotal:   4,
		},
		{
			name: "changed recipe cannot be read",
			fail: func(e *env) {
				e.fake.SeedRecipe(testsupport.Recipe("R2", "Apple Crumble", "apples\noats"))
				failRecipe(e, "R2", 1)
			},
			wantIndexed: 2,
			wantTotal:   3,
		},
		{
			name: "recipe list cannot be read",
			fail: func(e *env) {
				e.fake.Inject(fakepaprika.Fault{
					Method:  "GET",
					Path:    recipesPath,
					Kind:    fakepaprika.FaultStatus,
					Status:  500,
					Message: "boom",
				})
			},
			wantIndexed: 3,
			wantTotal:   3,
		},
	} {
		t.Run(tt.name, func(t *testing.T) {
			e := newEnv(t)
			seedThree(e)
			c := e.newCache(nil)
			if err := c.Sync(ctx); err != nil {
				t.Fatal(err)
			}
			tt.fail(e)
			if err := c.Sync(ctx); err == nil {
				t.Fatal("expected the pass to fail")
			}
			p := c.Progress()
			if !p.Ready || p.Indexed != tt.wantIndexed || p.Total != tt.wantTotal {
				t.Errorf(
					"progress = %+v, want ready with %d of %d",
					p,
					tt.wantIndexed,
					tt.wantTotal,
				)
			}
			if _, err := c.Search(ctx, caching.Query{}); err != nil {
				t.Errorf("search after the failed pass: %v", err)
			}
		})
	}
}

func TestExpiryDoesNotMakeReadyCacheUnready(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	seedThree(e)
	c := e.newCache(nil)
	if err := c.Sync(ctx); err != nil {
		t.Fatal(err)
	}

	e.clock.Advance(31 * 24 * time.Hour)
	failRecipe(e, "R2", 1)
	if err := c.Sync(ctx); err == nil {
		t.Fatal("expected the full re-read to fail")
	}
	if p := c.Progress(); !p.Ready || p.Indexed != 3 || p.Total != 3 {
		t.Errorf("progress = %+v, want ready with 3 of 3", p)
	}
	if _, err := c.Search(ctx, caching.Query{}); err != nil {
		t.Errorf("search during the full re-read: %v", err)
	}
}

func TestPartialSnapshotProgressFollowsLiveList(t *testing.T) {
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
	if snapshotComplete(t, first) {
		t.Error("a partial index must not be saved as complete")
	}
	saveSnapshot(t, first, store)
	gate.fail(errors.New("stop"))
	<-errc

	gate2 := newGatedSource(e.src, "R2")
	second := caching.New(
		gate2,
		caching.Options{
			Now:    e.clock.Now,
			Store:  store,
			Logger: slog.New(slog.NewTextHandler(&bytes.Buffer{}, nil)),
		},
	)
	second.LoadSnapshot(ctx)
	if p := second.Progress(); p.Ready || p.Indexed != 0 || p.Total != 0 || p.Recipes != 1 {
		t.Errorf("before the list is read: %+v, want nothing counted yet", p)
	}

	done := make(chan error, 1)
	go func() { done <- second.Sync(ctx) }()
	gate2.waitBlocked(t)
	if p := second.Progress(); p.Ready || p.Indexed != 1 || p.Total != 3 {
		t.Errorf("after the list is read: %+v, want 1 of 3 at once", p)
	}
	_, err := second.Search(ctx, caching.Query{})
	var loading *caching.LoadingError
	if !errors.As(err, &loading) || !strings.Contains(loading.Error(), "1 of 3") {
		t.Errorf("search error = %v, want cumulative progress", err)
	}
	gate2.open()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if p := second.Progress(); !p.Ready || p.Indexed != 3 {
		t.Errorf("after the pass: %+v, want ready", p)
	}
}

func TestCompleteSnapshotIsReadyBeforeComparison(t *testing.T) {
	ctx := context.Background()
	e := newEnv(t)
	seedThree(e)
	store, _ := newFileStore(t)
	first := e.newCache(store)
	if err := first.Sync(ctx); err != nil {
		t.Fatal(err)
	}
	saveSnapshot(t, first, store)

	// Paprika gained a recipe while the server was down. The loaded index
	// serves search at once, and the comparison then counts the newcomer.
	e.fake.SeedRecipe(testsupport.Recipe("R4", "Fresh Bread", "flour"))
	failRecipe(e, "R4", 1)
	second := e.newCache(store)
	second.LoadSnapshot(ctx)
	if p := second.Progress(); !p.Ready {
		t.Fatalf("after load: %+v, want ready", p)
	}
	if err := second.Sync(ctx); err == nil {
		t.Fatal("expected the pass to fail")
	}
	if p := second.Progress(); !p.Ready || p.Indexed != 3 || p.Total != 4 {
		t.Errorf("after the comparison: %+v, want ready with 3 of 4", p)
	}
}
