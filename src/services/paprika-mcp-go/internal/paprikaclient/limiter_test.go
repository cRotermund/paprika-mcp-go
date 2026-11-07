package paprikaclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"strings"
	"testing"
	"time"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient/fakepaprika"
)

// limitedClient returns a client with the token bucket on and a fake clock
// that sleeping advances.
func limitedClient(
	t *testing.T,
	f *fakepaprika.Server,
	mutate func(*Config),
) (*Client, *stepClock) {
	t.Helper()
	sc := newStepClock()
	c, _ := newTestClient(t, f, func(cfg *Config) {
		cfg.RequestsPerMinute = 60
		cfg.RequestBurst = 8
		cfg.Now = func() time.Time { return sc.now }
		if mutate != nil {
			mutate(cfg)
		}
	})
	c.bucket.clock = sc.clock()
	return c, sc
}

func TestNewRejectsBurstAtOrBelowReserve(t *testing.T) {
	tests := []struct {
		name    string
		perMin  int
		burst   int
		wantErr bool
	}{
		{"burst equal to the reserve", 60, BackgroundReserve, true},
		{"burst below the reserve", 60, 1, true},
		{"burst above the reserve", 60, BackgroundReserve + 1, false},
		{"limiting off ignores the burst", -1, 1, false},
		{"defaults", 0, 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(Config{
				Email:             "e",
				Password:          "p",
				RequestsPerMinute: tt.perMin,
				RequestBurst:      tt.burst,
			})
			if tt.wantErr != errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error = %v, want invalid argument = %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoginsAndRequestsShareTheBurst(t *testing.T) {
	f := newFake(t)
	c, sc := limitedClient(t, f, nil)
	// One login and nine reads make ten requests against a burst of eight.
	for range 9 {
		if _, err := c.Status(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(f.Requests()); got != 10 {
		t.Fatalf("requests = %d, want 10", got)
	}
	if got := sc.total(); got != 2*time.Second {
		t.Fatalf("waited %s, want 2s for the two requests past the burst", got)
	}
}

func TestBackgroundStopsAtTheReserve(t *testing.T) {
	f := newFake(t)
	c, sc := limitedClient(t, f, nil)
	bg := Background(context.Background())

	// The login takes 1 of 8 tokens; background requests may use tokens until
	// only the reserve is left, which is five more.
	for i := range 5 {
		if _, err := c.Status(bg); err != nil {
			t.Fatalf("background request %d: %v", i+1, err)
		}
	}
	if len(sc.slept) != 0 {
		t.Fatalf("background requests waited before the reserve: %v", sc.slept)
	}

	blocked, cancel := context.WithCancel(bg)
	clock := sc.clock()
	clock.sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	c.bucket.clock = clock
	before := len(f.Requests())
	if _, err := c.Status(blocked); !errors.Is(err, context.Canceled) {
		t.Fatalf("background error = %v, want it held until canceled", err)
	}

	if _, err := c.Status(context.Background()); err != nil {
		t.Fatalf("interactive request with the reserve left: %v", err)
	}
	if got := len(f.Requests()) - before; got != 1 {
		t.Fatalf("requests after the hold = %d, want only the interactive one", got)
	}
}

// logRecords decodes JSON log lines.
func logRecords(t *testing.T, buf *bytes.Buffer) []map[string]any {
	t.Helper()
	var out []map[string]any
	for _, line := range strings.Split(strings.TrimSpace(buf.String()), "\n") {
		if line == "" {
			continue
		}
		rec := map[string]any{}
		if err := json.Unmarshal([]byte(line), &rec); err != nil {
			t.Fatalf("log line %q: %v", line, err)
		}
		out = append(out, rec)
	}
	return out
}

func TestTimeoutEmptiesBucketAndDelaysNextRequest(t *testing.T) {
	f := newFake(t)
	const password = "hunter2-very-secret"
	f.SetPassword(password)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	c, sc := limitedClient(t, f, func(cfg *Config) {
		cfg.Logger = logger
		cfg.Password = password
		cfg.RequestTimeout = 30 * time.Millisecond
		cfg.StallCooldown = 90 * time.Second
	})
	ctx := context.Background()

	f.Inject(fakepaprika.Fault{
		Path:  statusPath,
		Kind:  fakepaprika.FaultDelay,
		Delay: 300 * time.Millisecond,
	})
	if _, err := c.Status(ctx); !errors.Is(err, ErrTemporary) {
		t.Fatalf("error = %v, want ErrTemporary", err)
	}
	if len(sc.slept) != 0 {
		t.Fatalf("the first requests waited: %v", sc.slept)
	}

	if _, err := c.Status(ctx); err != nil {
		t.Fatal(err)
	}
	if got := sc.total(); got != 90*time.Second {
		t.Fatalf("next request waited %s, want the 90s cooldown", got)
	}

	var stall map[string]any
	for _, rec := range logRecords(t, &logs) {
		if strings.Contains(rec["msg"].(string), "stalled") {
			stall = rec
		}
	}
	if stall == nil {
		t.Fatalf("no stall warning in the logs:\n%s", logs.String())
	}
	want := map[string]any{
		"level":                "WARN",
		"requests_last_minute": float64(2),
		"requests_per_minute":  float64(60),
		"burst":                float64(8),
		"method":               "GET",
		"path":                 statusPath,
	}
	for k, v := range want {
		if stall[k] != v {
			t.Errorf("stall %s = %v, want %v", k, stall[k], v)
		}
	}
	for _, secret := range []string{password, f.Email(), "Bearer", "fake-token-"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs contain %q:\n%s", secret, logs.String())
		}
	}
}

func TestThreeTimeoutsStillPauseWithLimiting(t *testing.T) {
	f := newFake(t)
	c, sc := limitedClient(t, f, func(cfg *Config) {
		cfg.RequestTimeout = 30 * time.Millisecond
		cfg.TimeoutThreshold = 3
		cfg.TimeoutPause = 5 * time.Minute
	})
	ctx := context.Background()
	f.Inject(fakepaprika.Fault{
		Path:  statusPath,
		Times: -1,
		Kind:  fakepaprika.FaultDelay,
		Delay: 300 * time.Millisecond,
	})
	for i := range 3 {
		if _, err := c.Status(ctx); !errors.Is(err, ErrTemporary) {
			t.Fatalf("timeout %d: error = %v, want ErrTemporary", i+1, err)
		}
	}
	before, slept := len(f.Requests()), sc.total()
	if _, err := c.Status(ctx); !errors.Is(err, ErrTemporary) {
		t.Fatalf("paused call: error = %v, want ErrTemporary", err)
	}
	if len(f.Requests()) != before || sc.total() != slept {
		t.Fatal("a paused call must neither reach the server nor take a token")
	}

	f.ClearFaults()
	sc.now = sc.now.Add(5*time.Minute + time.Second)
	if _, err := c.Status(ctx); err != nil {
		t.Fatalf("after the pause: %v", err)
	}
}

func TestWriteTimeoutWithLimitingIsUnknownAndSentOnce(t *testing.T) {
	f := newFake(t)
	c, _ := limitedClient(t, f, func(cfg *Config) { cfg.RequestTimeout = 30 * time.Millisecond })
	f.Inject(fakepaprika.Fault{
		Method: "POST",
		Path:   mealsPath,
		Times:  -1,
		Kind:   fakepaprika.FaultDelay,
		Delay:  300 * time.Millisecond,
	})
	meal := Object{}
	_ = meal.Set("uid", "MEAL-1")
	err := c.SaveMeals(context.Background(), []Object{meal})
	if !errors.Is(err, ErrOutcomeUnknown) || errors.Is(err, ErrTemporary) {
		t.Fatalf("error = %v, want ErrOutcomeUnknown only", err)
	}
	if got := len(f.RequestsTo("POST", mealsPath)); got != 1 {
		t.Fatalf("meal POSTs = %d, want exactly 1", got)
	}
}
