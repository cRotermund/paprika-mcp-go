package paprikaclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient/fakepaprika"
)

const (
	statusPath = "/api/v2/sync/status/"
	mealsPath  = "/api/v2/sync/meals/"
	notifyPath = "/api/v2/sync/notify/"
	loginURL   = "/api/v1/account/login/"
)

type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newFakeClock() *fakeClock {
	return &fakeClock{t: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
}

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

// newTestClient returns a client for the fake with pacing off, a fake clock,
// and a generous timeout. mutate may adjust the config.
func newTestClient(t *testing.T, f *fakepaprika.Server, mutate func(*Config)) (*Client, *fakeClock) {
	t.Helper()
	clock := newFakeClock()
	cfg := Config{
		BaseURL:        f.URL(),
		Email:          f.Email(),
		Password:       f.Password(),
		UserAgent:      "paprika-mcp-go/test",
		Logger:         discardLogger(),
		RequestTimeout: 5 * time.Second,
		MinInterval:    -1,
		Now:            clock.Now,
	}
	if mutate != nil {
		mutate(&cfg)
	}
	c, err := New(cfg)
	if err != nil {
		t.Fatalf("New: %v", err)
	}
	return c, clock
}

func newFake(t *testing.T) *fakepaprika.Server {
	t.Helper()
	f := fakepaprika.New(t)
	f.SeedDefault()
	return f
}

func TestNewValidatesConfig(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
	}{
		{"no email", Config{Password: "p"}},
		{"no password", Config{Email: "e"}},
		{"plain http to a remote host", Config{Email: "e", Password: "p", BaseURL: "http://www.paprikaapp.com"}},
		{"unsupported scheme", Config{Email: "e", Password: "p", BaseURL: "ftp://127.0.0.1"}},
		{"credentials in URL", Config{Email: "e", Password: "p", BaseURL: "https://user:pw@example.test"}},
		{"query in URL", Config{Email: "e", Password: "p", BaseURL: "https://example.test/?a=b"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			if _, err := New(tt.cfg); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("New error = %v, want ErrInvalidArgument", err)
			}
		})
	}

	if _, err := New(Config{Email: "e", Password: "p"}); err != nil {
		t.Fatalf("New with defaults: %v", err)
	}
}

func TestLoginOnceAndTokenReuse(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, nil)
	ctx := context.Background()

	for range 3 {
		if _, err := c.Status(ctx); err != nil {
			t.Fatalf("Status: %v", err)
		}
	}
	if got := f.LoginCount(); got != 1 {
		t.Fatalf("logins = %d, want 1", got)
	}
	reqs := f.Requests()
	if len(reqs) != 4 {
		t.Fatalf("requests = %d, want login plus 3 reads", len(reqs))
	}
	if reqs[0].Path != loginURL || reqs[0].Email != f.Email() {
		t.Fatalf("first request = %s email %q", reqs[0].Path, reqs[0].Email)
	}
	for _, r := range reqs[1:] {
		if !r.Authorized || r.Header.Get("Authorization") != "[redacted]" {
			t.Fatalf("read not authorized: %+v", r)
		}
		if r.Header.Get("User-Agent") != "paprika-mcp-go/test" || r.Header.Get("Accept-Encoding") != "gzip" {
			t.Fatalf("headers = %v", r.Header)
		}
	}
}

func TestRelogin(t *testing.T) {
	t.Run("401 then one login and one retry", func(t *testing.T) {
		f := newFake(t)
		c, _ := newTestClient(t, f, nil)
		ctx := context.Background()
		if _, err := c.Status(ctx); err != nil {
			t.Fatal(err)
		}
		f.RevokeTokens()
		f.ResetRequests()

		if _, err := c.Status(ctx); err != nil {
			t.Fatalf("Status after revoke: %v", err)
		}
		var got []string
		for _, r := range f.Requests() {
			got = append(got, fmt.Sprintf("%s %s %d", r.Method, r.Path, r.Status))
		}
		want := []string{
			"GET " + statusPath + " 401",
			"POST " + loginURL + " 200",
			"GET " + statusPath + " 200",
		}
		if strings.Join(got, "|") != strings.Join(want, "|") {
			t.Fatalf("requests = %v, want %v", got, want)
		}
	})

	t.Run("second 401 gives up", func(t *testing.T) {
		f := newFake(t)
		c, _ := newTestClient(t, f, nil)
		f.Inject(fakepaprika.Fault{Method: "GET", Path: statusPath, Times: -1, Kind: fakepaprika.FaultUnauthorized})

		_, err := c.Status(context.Background())
		if !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("error = %v, want ErrUnauthorized", err)
		}
		if got := len(f.RequestsTo("GET", statusPath)); got != 2 {
			t.Fatalf("status requests = %d, want 2", got)
		}
		if got := f.LoginCount(); got != 2 {
			t.Fatalf("logins = %d, want 2", got)
		}
	})

	t.Run("a write is retried once after 401 only", func(t *testing.T) {
		f := newFake(t)
		c, _ := newTestClient(t, f, nil)
		ctx := context.Background()
		if _, err := c.Status(ctx); err != nil {
			t.Fatal(err)
		}
		f.RevokeTokens()

		meal := Object{}
		_ = meal.Set("uid", "MEAL-1")
		if err := c.SaveMeals(ctx, []Object{meal}); err != nil {
			t.Fatalf("SaveMeals: %v", err)
		}
		posts := f.RequestsTo("POST", mealsPath)
		if len(posts) != 2 || posts[0].Status != 401 || posts[1].Status != 200 {
			t.Fatalf("meal POSTs = %+v", posts)
		}
		if got := f.Counters()["meals"]; got != 1 {
			t.Fatalf("meals counter = %d, want 1", got)
		}
	})
}

func TestLoginBackoff(t *testing.T) {
	f := newFake(t)
	c, clock := newTestClient(t, f, func(cfg *Config) {
		cfg.LoginBackoffMin = time.Minute
		cfg.LoginBackoffMax = 4 * time.Minute
	})
	accountPassword := f.Password()
	f.SetPassword("changed-on-the-account")
	ctx := context.Background()
	attempt := func() error {
		_, err := c.Status(ctx)
		return err
	}

	logins := 0
	for _, wait := range []time.Duration{time.Minute, 2 * time.Minute, 4 * time.Minute, 4 * time.Minute} {
		if err := attempt(); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("attempt error = %v, want ErrUnauthorized", err)
		}
		logins++
		if got := f.LoginCount(); got != logins {
			t.Fatalf("logins = %d, want %d", got, logins)
		}

		clock.Advance(wait - time.Second)
		if err := attempt(); !errors.Is(err, ErrUnauthorized) {
			t.Fatalf("blocked attempt error = %v", err)
		}
		if got := f.LoginCount(); got != logins {
			t.Fatalf("login sent during backoff of %s: %d", wait, got)
		}
		clock.Advance(2 * time.Second)
	}

	f.SetPassword(accountPassword)
	if err := attempt(); err != nil {
		t.Fatalf("attempt after the backoff with a working password: %v", err)
	}
	if got := len(f.RequestsTo("GET", statusPath)); got != 1 {
		t.Fatalf("status requests = %d, want 1 (only after login succeeded)", got)
	}
}

func TestLoginTransientFailureDoesNotBackOff(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, nil)
	f.Inject(fakepaprika.Fault{Path: loginURL, Kind: fakepaprika.FaultUnavailable})

	if _, err := c.Status(context.Background()); !errors.Is(err, ErrTemporary) {
		t.Fatalf("error = %v, want ErrTemporary", err)
	}
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatalf("second attempt should log in at once: %v", err)
	}
	if got := f.LoginCount(); got != 1 {
		t.Fatalf("logins = %d, want 1 (the 503 was injected before the handler)", got)
	}
}

func TestSharedLogin(t *testing.T) {
	tests := []struct {
		name     string
		badLogin bool
		wantErr  error
	}{
		{"success", false, nil},
		{"failure", true, ErrUnauthorized},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			c, _ := newTestClient(t, f, nil)
			if tt.badLogin {
				f.SetPassword("other")
			}
			f.Inject(fakepaprika.Fault{Path: loginURL, Times: -1, Kind: fakepaprika.FaultDelay, Delay: 80 * time.Millisecond})

			const callers = 6
			errs := make(chan error, callers)
			for range callers {
				go func() {
					_, err := c.Status(context.Background())
					errs <- err
				}()
			}
			for range callers {
				err := <-errs
				if tt.wantErr == nil && err != nil {
					t.Errorf("Status: %v", err)
				}
				if tt.wantErr != nil && !errors.Is(err, tt.wantErr) {
					t.Errorf("error = %v, want %v", err, tt.wantErr)
				}
			}
			if got := f.LoginCount(); got != 1 {
				t.Fatalf("logins = %d, want 1 shared attempt", got)
			}
		})
	}
}

func TestPacing(t *testing.T) {
	const interval = 40 * time.Millisecond
	f := newFake(t)
	c, _ := newTestClient(t, f, func(cfg *Config) { cfg.MinInterval = interval })

	var wg sync.WaitGroup
	for range 3 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if _, err := c.Status(context.Background()); err != nil {
				t.Errorf("Status: %v", err)
			}
		}()
	}
	wg.Wait()

	reqs := f.Requests()
	if len(reqs) != 4 {
		t.Fatalf("requests = %d, want 4", len(reqs))
	}
	for i := 1; i < len(reqs); i++ {
		if gap := reqs[i].Time.Sub(reqs[i-1].Time); gap < interval-10*time.Millisecond {
			t.Errorf("gap before request %d = %s, want at least about %s", i, gap, interval)
		}
	}
	if got := f.MaxConcurrent(); got != 1 {
		t.Fatalf("max concurrent requests = %d, want 1", got)
	}
}

func TestPacingHonorsContext(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, func(cfg *Config) { cfg.MinInterval = 300 * time.Millisecond })
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := c.Status(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context deadline", err)
	}
}

func TestErrorMapping(t *testing.T) {
	notFound := Object{}
	_ = notFound.Set("uid", "X")
	tests := []struct {
		name  string
		fault fakepaprika.Fault
		call  func(c *Client) error
		check func(t *testing.T, err error)
	}{
		{
			name:  "error envelope on HTTP 200",
			fault: fakepaprika.Fault{Path: statusPath, Kind: fakepaprika.FaultEnvelopeError, Code: 7, Message: "boom"},
			call:  func(c *Client) error { _, err := c.Status(context.Background()); return err },
			check: func(t *testing.T, err error) {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.Status != 200 || apiErr.Code != 7 || apiErr.Message != "boom" {
					t.Fatalf("error = %#v", err)
				}
			},
		},
		{
			name:  "error envelope on a write",
			fault: fakepaprika.Fault{Path: mealsPath, Kind: fakepaprika.FaultEnvelopeError, Code: 3, Message: "no"},
			call:  func(c *Client) error { return c.SaveMeals(context.Background(), []Object{notFound}) },
			check: func(t *testing.T, err error) {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.Code != 3 {
					t.Fatalf("error = %#v", err)
				}
			},
		},
		{
			name: "an error envelope saying not found on a recipe read is not found",
			call: func(c *Client) error { _, err := c.GetRecipe(context.Background(), "missing"); return err },
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("error = %v, want ErrNotFound", err)
				}
			},
		},
		{
			name:  "404 is not found",
			fault: fakepaprika.Fault{Path: "/api/v2/sync/recipe/missing/", Kind: fakepaprika.FaultStatus, Status: 404, Message: "Not found."},
			call:  func(c *Client) error { _, err := c.GetRecipe(context.Background(), "missing"); return err },
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrNotFound) {
					t.Fatalf("error = %v, want ErrNotFound", err)
				}
			},
		},
		{
			name:  "503 on a read is temporary",
			fault: fakepaprika.Fault{Path: statusPath, Kind: fakepaprika.FaultUnavailable},
			call:  func(c *Client) error { _, err := c.Status(context.Background()); return err },
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrTemporary) {
					t.Fatalf("error = %v, want ErrTemporary", err)
				}
			},
		},
		{
			name:  "503 on a write is temporary",
			fault: fakepaprika.Fault{Path: notifyPath, Kind: fakepaprika.FaultUnavailable},
			call:  func(c *Client) error { return c.Notify(context.Background()) },
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrTemporary) || errors.Is(err, ErrOutcomeUnknown) {
					t.Fatalf("error = %v, want ErrTemporary only", err)
				}
			},
		},
		{
			name:  "429 is temporary",
			fault: fakepaprika.Fault{Path: statusPath, Kind: fakepaprika.FaultStatus, Status: 429},
			call:  func(c *Client) error { _, err := c.Status(context.Background()); return err },
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrTemporary) {
					t.Fatalf("error = %v, want ErrTemporary", err)
				}
			},
		},
		{
			name:  "500 on a read is temporary",
			fault: fakepaprika.Fault{Path: statusPath, Kind: fakepaprika.FaultStatus, Status: 500},
			call:  func(c *Client) error { _, err := c.Status(context.Background()); return err },
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrTemporary) {
					t.Fatalf("error = %v, want ErrTemporary", err)
				}
			},
		},
		{
			name:  "500 on a write leaves the outcome unknown",
			fault: fakepaprika.Fault{Path: mealsPath, Kind: fakepaprika.FaultStatus, Status: 500},
			call:  func(c *Client) error { return c.SaveMeals(context.Background(), []Object{notFound}) },
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrOutcomeUnknown) {
					t.Fatalf("error = %v, want ErrOutcomeUnknown", err)
				}
			},
		},
		{
			name:  "unexpected 4xx is an API error",
			fault: fakepaprika.Fault{Path: statusPath, Kind: fakepaprika.FaultStatus, Status: 418, Code: 9},
			call:  func(c *Client) error { _, err := c.Status(context.Background()); return err },
			check: func(t *testing.T, err error) {
				var apiErr *APIError
				if !errors.As(err, &apiErr) || apiErr.Status != 418 || apiErr.Code != 9 {
					t.Fatalf("error = %#v", err)
				}
			},
		},
		{
			name:  "garbage body",
			fault: fakepaprika.Fault{Path: statusPath, Kind: fakepaprika.FaultBody, Body: []byte("<html>")},
			call:  func(c *Client) error { _, err := c.Status(context.Background()); return err },
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrUnexpectedResponse) {
					t.Fatalf("error = %v, want ErrUnexpectedResponse", err)
				}
			},
		},
		{
			name:  "write not confirmed",
			fault: fakepaprika.Fault{Path: notifyPath, Kind: fakepaprika.FaultBody, Body: []byte(`{"result":false}`)},
			call:  func(c *Client) error { return c.Notify(context.Background()) },
			check: func(t *testing.T, err error) {
				if !errors.Is(err, ErrUnexpectedResponse) {
					t.Fatalf("error = %v, want ErrUnexpectedResponse", err)
				}
			},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			c, _ := newTestClient(t, f, nil)
			if tt.fault.Kind != 0 {
				f.Inject(tt.fault)
			}
			tt.check(t, tt.call(c))
		})
	}
}

func TestTimeoutPause(t *testing.T) {
	f := newFake(t)
	c, clock := newTestClient(t, f, func(cfg *Config) {
		cfg.RequestTimeout = 30 * time.Millisecond
		cfg.TimeoutThreshold = 3
		cfg.TimeoutPause = 5 * time.Minute
	})
	ctx := context.Background()
	f.Inject(fakepaprika.Fault{Path: statusPath, Times: -1, Kind: fakepaprika.FaultDelay, Delay: 300 * time.Millisecond})

	for i := range 3 {
		if _, err := c.Status(ctx); !errors.Is(err, ErrTemporary) {
			t.Fatalf("timeout %d: error = %v, want ErrTemporary", i+1, err)
		}
	}
	before := len(f.Requests())
	if _, err := c.Status(ctx); !errors.Is(err, ErrTemporary) {
		t.Fatalf("paused call: error = %v, want ErrTemporary", err)
	}
	if after := len(f.Requests()); after != before {
		t.Fatalf("paused call reached the server: %d new requests", after-before)
	}

	f.ClearFaults()
	clock.Advance(5*time.Minute + time.Second)
	if _, err := c.Status(ctx); err != nil {
		t.Fatalf("after the pause: %v", err)
	}
}

func TestTimeoutCountResetsOnSuccess(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, func(cfg *Config) {
		cfg.RequestTimeout = 30 * time.Millisecond
		cfg.TimeoutThreshold = 2
	})
	ctx := context.Background()
	for range 3 {
		f.Inject(fakepaprika.Fault{Path: statusPath, Kind: fakepaprika.FaultDelay, Delay: 300 * time.Millisecond})
		if _, err := c.Status(ctx); !errors.Is(err, ErrTemporary) {
			t.Fatalf("error = %v, want ErrTemporary", err)
		}
		if _, err := c.Status(ctx); err != nil {
			t.Fatalf("a success between timeouts must not hit a pause: %v", err)
		}
	}
}

func TestWriteTimeoutLeavesOutcomeUnknownWithoutRetry(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, func(cfg *Config) { cfg.RequestTimeout = 30 * time.Millisecond })
	f.Inject(fakepaprika.Fault{Method: "POST", Path: mealsPath, Times: -1, Kind: fakepaprika.FaultDelay, Delay: 300 * time.Millisecond})

	meal := Object{}
	_ = meal.Set("uid", "MEAL-1")
	err := c.SaveMeals(context.Background(), []Object{meal})
	if !errors.Is(err, ErrOutcomeUnknown) {
		t.Fatalf("error = %v, want ErrOutcomeUnknown", err)
	}
	if errors.Is(err, ErrTemporary) {
		t.Fatalf("a timed-out write must not look retryable: %v", err)
	}
	if got := len(f.RequestsTo("POST", mealsPath)); got != 1 {
		t.Fatalf("meal POSTs = %d, want exactly 1", got)
	}
}

func TestRedirectIsNotFollowed(t *testing.T) {
	var hits atomic.Int32
	other := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hits.Add(1)
	}))
	t.Cleanup(other.Close)

	f := newFake(t)
	c, _ := newTestClient(t, f, nil)
	f.Inject(fakepaprika.Fault{Path: statusPath, Kind: fakepaprika.FaultRedirect, Location: other.URL + "/steal"})

	_, err := c.Status(context.Background())
	var apiErr *APIError
	if !errors.As(err, &apiErr) || apiErr.Status != http.StatusFound {
		t.Fatalf("error = %#v, want APIError with status 302", err)
	}
	if hits.Load() != 0 {
		t.Fatal("redirect target was contacted")
	}
}

func TestResponseSizeCap(t *testing.T) {
	big := []byte(`{"result":"` + strings.Repeat("x", 5000) + `"}`)
	tests := []struct {
		name string
		gzip bool
	}{
		{"plain", false},
		{"gzip that expands past the cap", true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			c, _ := newTestClient(t, f, func(cfg *Config) { cfg.MaxResponseBytes = 1024 })
			f.Inject(fakepaprika.Fault{Path: statusPath, Kind: fakepaprika.FaultBody, Body: big, Gzip: tt.gzip})

			if _, err := c.Status(context.Background()); !errors.Is(err, ErrResponseTooLarge) {
				t.Fatalf("error = %v, want ErrResponseTooLarge", err)
			}
		})
	}

	t.Run("gzip within the cap is decoded", func(t *testing.T) {
		f := newFake(t)
		c, _ := newTestClient(t, f, nil)
		f.Inject(fakepaprika.Fault{Path: statusPath, Kind: fakepaprika.FaultBody, Body: []byte(`{"result":{"recipes":5}}`), Gzip: true})
		got, err := c.Status(context.Background())
		if err != nil || got["recipes"] != 5 {
			t.Fatalf("Status = %v, %v", got, err)
		}
	})
}

func TestReads(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, nil)
	ctx := context.Background()

	counters, err := c.Status(ctx)
	if err != nil || counters["recipes"] != 2 || counters["categories"] != 2 {
		t.Fatalf("Status = %v, %v", counters, err)
	}

	refs, err := c.ListRecipes(ctx)
	if err != nil || len(refs) != 2 {
		t.Fatalf("ListRecipes = %v, %v", refs, err)
	}
	if refs[0].UID != fakepaprika.RecipeSoupUID || refs[0].Hash == "" {
		t.Fatalf("first ref = %+v", refs[0])
	}

	recipe, err := c.GetRecipe(ctx, fakepaprika.RecipeTrashedUID)
	if err != nil {
		t.Fatalf("GetRecipe: %v", err)
	}
	if recipe.Value.Name != "Old Stew" || !recipe.Value.InTrash || len(recipe.Value.Categories) != 1 {
		t.Fatalf("typed view = %+v", recipe.Value)
	}
	if !recipe.Raw.Has("future_field") || !recipe.Raw.Has("photo_large") {
		t.Fatalf("raw object lost fields: %v", recipe.Raw)
	}

	lists := []struct {
		name string
		path string
		want int
		call func() (int, error)
	}{
		{"categories", "categories", 2, func() (int, error) { v, err := c.Categories(ctx); return len(v), err }},
		{"meals", "meals", 0, func() (int, error) { v, err := c.Meals(ctx); return len(v), err }},
		{"mealtypes", "mealtypes", 4, func() (int, error) { v, err := c.MealTypes(ctx); return len(v), err }},
		{"grocerylists", "grocerylists", 1, func() (int, error) { v, err := c.GroceryLists(ctx); return len(v), err }},
		{"groceries", "groceries", 1, func() (int, error) { v, err := c.Groceries(ctx); return len(v), err }},
		{"groceryaisles", "groceryaisles", 1, func() (int, error) { v, err := c.GroceryAisles(ctx); return len(v), err }},
		{"groceryingredients", "groceryingredients", 0, func() (int, error) { v, err := c.GroceryIngredients(ctx); return len(v), err }},
	}
	for _, l := range lists {
		t.Run(l.name, func(t *testing.T) {
			f.ResetRequests()
			got, err := l.call()
			if err != nil || got != l.want {
				t.Fatalf("got %d, %v; want %d", got, err, l.want)
			}
			if n := len(f.RequestsTo("GET", "/api/v2/sync/"+l.path+"/")); n != 1 {
				t.Fatalf("GETs to %s = %d, want 1", l.path, n)
			}
		})
	}

	cats, _ := c.Categories(ctx)
	if cats[0].Value.Name != "Dinner" || cats[0].Value.ParentUID != nil {
		t.Fatalf("category = %+v", cats[0].Value)
	}
	items, _ := c.Groceries(ctx)
	if items[0].Value.ListUID != fakepaprika.GroceryListUID || items[0].Raw.String("aisle") != "Produce" {
		t.Fatalf("grocery = %+v", items[0].Value)
	}
}

func TestGetRecipeRejectsEmptyUID(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, nil)
	if _, err := c.GetRecipe(context.Background(), ""); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("error = %v, want ErrInvalidArgument", err)
	}
	if len(f.Requests()) != 0 {
		t.Fatal("an invalid call reached the server")
	}
}

func decodeBody(t *testing.T, r fakepaprika.Request) any {
	t.Helper()
	var v any
	if err := json.Unmarshal(r.Body, &v); err != nil {
		t.Fatalf("recorded body: %v", err)
	}
	return v
}

func TestSaveRecipeEncodingAndRoundTrip(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, nil)
	ctx := context.Background()

	entry, err := c.GetRecipe(ctx, fakepaprika.RecipeSoupUID)
	if err != nil {
		t.Fatal(err)
	}
	edited := entry.Raw.Clone()
	if err := edited.Set("name", "Spicy Carrot Soup"); err != nil {
		t.Fatal(err)
	}
	if err := edited.Set("hash", strings.Repeat("c", 64)); err != nil {
		t.Fatal(err)
	}
	delete(edited, "notes")
	f.ResetRequests()

	if err := c.SaveRecipe(ctx, fakepaprika.RecipeSoupUID, edited); err != nil {
		t.Fatalf("SaveRecipe: %v", err)
	}

	posts := f.RequestsTo("POST", "/api/v2/sync/recipe/"+fakepaprika.RecipeSoupUID+"/")
	if len(posts) != 1 {
		t.Fatalf("POSTs = %d, want 1", len(posts))
	}
	post := posts[0]
	if ct := post.Header.Get("Content-Type"); !strings.HasPrefix(ct, "multipart/form-data; boundary=") {
		t.Fatalf("Content-Type = %q", ct)
	}
	if len(post.Parts) != 1 || post.Parts[0].Name != "data" || post.Parts[0].Filename != "file" || !post.Parts[0].Gzipped {
		t.Fatalf("parts = %+v", post.Parts)
	}
	if post.BodyError != "" {
		t.Fatalf("body error: %s", post.BodyError)
	}
	body, ok := decodeBody(t, post).(map[string]any)
	if !ok || body["name"] != "Spicy Carrot Soup" || body["future_field"] == nil {
		t.Fatalf("body = %v", body)
	}

	stored, _ := f.StoredRecipe(fakepaprika.RecipeSoupUID)
	if stored["name"] != "Spicy Carrot Soup" || stored["hash"] != strings.Repeat("c", 64) || stored["notes"] != "" {
		t.Fatalf("stored = %v", stored)
	}
	if stored["future_field"] == nil {
		t.Fatal("unknown field was lost")
	}
	if got := f.Counters()["recipes"]; got != 3 {
		t.Fatalf("recipes counter = %d, want 3", got)
	}
}

func TestSaveRecipeRejectsMismatchedUID(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, nil)
	recipe := Object{}
	_ = recipe.Set("uid", "OTHER")
	if err := c.SaveRecipe(context.Background(), "REC", recipe); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("error = %v, want ErrInvalidArgument", err)
	}
	if err := c.SaveRecipe(context.Background(), "", recipe); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("error = %v, want ErrInvalidArgument", err)
	}
	if len(f.Requests()) != 0 {
		t.Fatal("an invalid call reached the server")
	}
}

func TestBulkSaves(t *testing.T) {
	tests := []struct {
		name     string
		resource string
		save     func(c *Client, objs []Object) error
	}{
		{"meals", "meals", func(c *Client, o []Object) error { return c.SaveMeals(context.Background(), o) }},
		{"groceries", "groceries", func(c *Client, o []Object) error { return c.SaveGroceryItems(context.Background(), o) }},
		{"grocerylists", "grocerylists", func(c *Client, o []Object) error { return c.SaveGroceryLists(context.Background(), o) }},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			f := newFake(t)
			c, _ := newTestClient(t, f, nil)
			before := f.Counters()[tt.resource]
			baseline := len(f.Objects(tt.resource))

			a, b := Object{}, Object{}
			_ = a.Set("uid", "NEW-A")
			_ = a.Set("name", "first")
			_ = a.Set("unmodeled", map[string]int{"n": 1})
			_ = b.Set("uid", "NEW-B")
			_ = b.Set("name", "second")
			if err := tt.save(c, []Object{a, b}); err != nil {
				t.Fatalf("save: %v", err)
			}

			if got := f.Counters()[tt.resource] - before; got != 2 {
				t.Fatalf("counter rose by %d, want 2", got)
			}
			if got := len(f.Objects(tt.resource)); got != baseline+2 {
				t.Fatalf("objects = %d, want %d", got, baseline+2)
			}
			posts := f.RequestsTo("POST", "/api/v2/sync/"+tt.resource+"/")
			if len(posts) != 1 || len(posts[0].Parts) != 1 || posts[0].Parts[0].Name != "data" || !posts[0].Parts[0].Gzipped {
				t.Fatalf("posts = %+v", posts)
			}
			arr, ok := decodeBody(t, posts[0]).([]any)
			if !ok || len(arr) != 2 {
				t.Fatalf("body = %s", posts[0].Body)
			}
			if arr[0].(map[string]any)["unmodeled"] == nil {
				t.Fatal("unmodeled field missing from the upload")
			}
		})
	}

	t.Run("validation", func(t *testing.T) {
		f := newFake(t)
		c, _ := newTestClient(t, f, nil)
		noUID := Object{}
		_ = noUID.Set("name", "x")
		if err := c.SaveMeals(context.Background(), []Object{noUID}); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("error = %v, want ErrInvalidArgument", err)
		}
		if err := c.SaveMeals(context.Background(), nil); err != nil {
			t.Fatalf("empty save: %v", err)
		}
		if len(f.Requests()) != 0 {
			t.Fatal("an empty or invalid save reached the server")
		}
	})
}

func TestNotify(t *testing.T) {
	f := newFake(t)
	c, _ := newTestClient(t, f, nil)
	if err := c.Notify(context.Background()); err != nil {
		t.Fatalf("Notify: %v", err)
	}
	posts := f.RequestsTo("POST", notifyPath)
	if len(posts) != 1 {
		t.Fatalf("notify POSTs = %d", len(posts))
	}
	p := posts[0]
	if p.Body != nil || len(p.Parts) != 0 || p.Header.Get("Content-Type") != "" {
		t.Fatalf("notify must have no body: %+v", p)
	}
}

func TestLogsNeverContainSecrets(t *testing.T) {
	f := newFake(t)
	const password = "hunter2-very-secret"
	f.SetPassword(password)

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c, clock := newTestClient(t, f, func(cfg *Config) {
		cfg.Logger = logger
		cfg.Password = password
		cfg.RequestTimeout = 30 * time.Millisecond
		cfg.TimeoutThreshold = 1
	})
	ctx := context.Background()

	meal := Object{}
	_ = meal.Set("uid", "MEAL-1")
	_ = meal.Set("name", "private-meal-name")
	if err := c.SaveMeals(ctx, []Object{meal}); err != nil {
		t.Fatal(err)
	}
	if _, err := c.Status(ctx); err != nil {
		t.Fatal(err)
	}
	f.RevokeTokens()
	if _, err := c.Status(ctx); err != nil {
		t.Fatal(err)
	}
	f.Inject(fakepaprika.Fault{Path: statusPath, Kind: fakepaprika.FaultDelay, Delay: 300 * time.Millisecond})
	_, _ = c.Status(ctx)
	clock.Advance(time.Hour)
	f.SetPassword("changed")
	f.RevokeTokens()
	_, _ = c.Status(ctx)

	out := logs.String()
	if out == "" {
		t.Fatal("expected some log output")
	}
	secrets := []string{password, f.LastToken(), "fake-token-", "private-meal-name", f.Email(), "Bearer"}
	for _, s := range secrets {
		if s != "" && strings.Contains(out, s) {
			t.Errorf("log output contains %q:\n%s", s, out)
		}
	}
}
