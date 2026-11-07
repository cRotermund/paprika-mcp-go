package caching_test

import (
	"context"
	"io"
	"log/slog"
	"sync"
	"testing"
	"time"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/caching"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/testsupport"
)

// call is one request the cache made, and whether its context was marked
// background.
type call struct {
	name       string
	background bool
}

// recordingSource notes the priority of every call the cache makes.
type recordingSource struct {
	*paprikaclient.Client
	mu    sync.Mutex
	calls []call
}

func (r *recordingSource) note(ctx context.Context, name string) {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.calls = append(r.calls, call{name: name, background: paprikaclient.IsBackground(ctx)})
}

func (r *recordingSource) take() []call {
	r.mu.Lock()
	defer r.mu.Unlock()
	out := r.calls
	r.calls = nil
	return out
}

func (r *recordingSource) Status(ctx context.Context) (paprikaclient.Counters, error) {
	r.note(ctx, "status")
	return r.Client.Status(ctx)
}

func (r *recordingSource) ListRecipes(ctx context.Context) ([]paprikaclient.RecipeRef, error) {
	r.note(ctx, "list")
	return r.Client.ListRecipes(ctx)
}

func (r *recordingSource) GetRecipe(
	ctx context.Context,
	uid string,
) (paprikaclient.Entry[paprikaclient.Recipe], error) {
	r.note(ctx, "recipe")
	return r.Client.GetRecipe(ctx, uid)
}

func TestRecipePassesAreBackground(t *testing.T) {
	e := newEnv(t)
	seedThree(e)
	rec := &recordingSource{Client: e.src}
	c := caching.New(rec, caching.Options{
		Logger:   slog.New(slog.NewTextHandler(io.Discard, nil)),
		Now:      e.clock.Now,
		RetryMin: time.Millisecond,
		RetryMax: 5 * time.Millisecond,
	})
	ctx := context.Background()

	assertAll := func(t *testing.T, got []call, want map[string]bool) {
		t.Helper()
		if len(got) == 0 {
			t.Fatal("no calls recorded")
		}
		for _, g := range got {
			if g.background != want[g.name] {
				t.Errorf("%s background = %v, want %v", g.name, g.background, want[g.name])
			}
		}
	}

	t.Run("startup Run", func(t *testing.T) {
		c.Run(ctx)
		assertAll(t, rec.take(), map[string]bool{"status": true, "list": true, "recipe": true})
	})

	t.Run("pass started by a freshness check", func(t *testing.T) {
		e.fake.SeedRecipe(testsupport.Recipe("R4", "Pancakes", "flour\neggs"))
		e.clock.Advance(2 * time.Minute)
		if err := c.Fresh(ctx); err != nil {
			t.Fatal(err)
		}
		got := rec.take()
		if len(got) < 4 || got[0].name != "status" || got[0].background {
			t.Fatalf("the freshness check itself must be interactive: %+v", got)
		}
		assertAll(t, got[1:], map[string]bool{"status": true, "list": true, "recipe": true})
	})

	t.Run("age-based re-read", func(t *testing.T) {
		e.clock.Advance(31 * 24 * time.Hour)
		if err := c.Fresh(ctx); err != nil {
			t.Fatal(err)
		}
		got := rec.take()
		sawRecipe := false
		for _, g := range got[1:] {
			sawRecipe = sawRecipe || g.name == "recipe"
		}
		if !sawRecipe {
			t.Fatalf("expected age-based re-reads: %+v", got)
		}
		assertAll(t, got[1:], map[string]bool{"status": true, "list": true, "recipe": true})
	})

	t.Run("a tool call reading a recipe is interactive", func(t *testing.T) {
		e.fake.SeedRecipe(testsupport.Recipe("R5", "Waffles", "x"))
		rec.take()
		if _, err := c.Recipe(ctx, "R5"); err != nil {
			t.Fatal(err)
		}
		for _, g := range rec.take() {
			if g.name == "recipe" && g.background {
				t.Error("tool-call recipe read was marked background")
			}
		}
	})
}
