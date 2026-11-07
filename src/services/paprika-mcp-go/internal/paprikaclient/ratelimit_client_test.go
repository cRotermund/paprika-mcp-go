package paprikaclient

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient/fakepaprika"
)

// limitedClient returns a client with the sliding-window limiter on (the
// defaults: 10 starts per 12 s) and a fake clock that sleeping advances.
func limitedClient(
	t *testing.T,
	f *fakepaprika.Server,
	mutate func(*Config),
) (*Client, *stepClock) {
	t.Helper()
	sc := newStepClock()
	c, _ := newTestClient(t, f, func(cfg *Config) {
		cfg.RequestLimit = DefaultRequestLimit
		cfg.RequestWindow = DefaultRequestWindow
		cfg.Now = func() time.Time { return sc.now }
		if mutate != nil {
			mutate(cfg)
		}
	})
	c.limiter.clock = sc.clock()
	return c, sc
}

func TestNewRejectsLimitAtOrBelowReserve(t *testing.T) {
	tests := []struct {
		name    string
		limit   int
		wantErr bool
	}{
		{"limit equal to the reserve", BackgroundReserve, true},
		{"limit below the reserve", 1, true},
		{"limit above the reserve", BackgroundReserve + 1, false},
		{"limiting off ignores the limit", -1, false},
		{"defaults", 0, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			_, err := New(Config{
				Email:        "e",
				Password:     "p",
				RequestLimit: tt.limit,
			})
			if tt.wantErr != errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("error = %v, want invalid argument = %v", err, tt.wantErr)
			}
		})
	}
}

func TestLoginsAndRequestsShareTheWindow(t *testing.T) {
	f := newFake(t)
	c, sc := limitedClient(t, f, nil)
	// One login and nine reads make ten starts: the whole limit, with no wait.
	for range 9 {
		if _, err := c.Status(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(f.Requests()); got != 10 {
		t.Fatalf("requests = %d, want 10", got)
	}
	if len(sc.slept) != 0 {
		t.Fatalf("the first ten requests waited: %v", sc.slept)
	}

	// The eleventh waits until the login leaves the window.
	if _, err := c.Status(context.Background()); err != nil {
		t.Fatal(err)
	}
	if got := sc.total(); got != DefaultRequestWindow {
		t.Fatalf("waited %s, want the 12s window", got)
	}
}

func TestBackgroundStopsAtTheReserve(t *testing.T) {
	f := newFake(t)
	c, sc := limitedClient(t, f, nil)
	bg := Background(context.Background())

	// The login is one of 10 starts; background requests may start until only
	// the reserve is left, which is seven more.
	for i := range 7 {
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
	c.limiter.clock = clock
	before := len(f.Requests())
	if _, err := c.Status(blocked); !errors.Is(err, context.Canceled) {
		t.Fatalf("background error = %v, want it held until canceled", err)
	}

	c.limiter.clock = sc.clock()
	for range BackgroundReserve {
		if _, err := c.Status(context.Background()); err != nil {
			t.Fatalf("interactive request with the reserve left: %v", err)
		}
	}
	if got := len(f.Requests()) - before; got != BackgroundReserve {
		t.Fatalf("requests after the hold = %d, want only the interactive ones", got)
	}
	if len(sc.slept) != 0 {
		t.Fatalf("interactive requests waited: %v", sc.slept)
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

func TestTimeoutHoldsTheLimiterAndDelaysNextRequest(t *testing.T) {
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
		"level":              "WARN",
		"requests_in_window": float64(2),
		"request_limit":      float64(DefaultRequestLimit),
		"request_window":     float64(DefaultRequestWindow),
		"method":             "GET",
		"path":               statusPath,
		"cooldown":           float64(90 * time.Second),
		"reused":             true,
		"conn_requests":      float64(2),
	}
	for k, v := range want {
		if stall[k] != v {
			t.Errorf("stall %s = %v, want %v", k, stall[k], v)
		}
	}
	for _, name := range []string{"was_idle", "idle_time"} {
		if _, ok := stall[name]; !ok {
			t.Errorf("stall has no %s", name)
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
		t.Fatal("a paused call must neither reach the server nor wait on the limiter")
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

func TestDebugLogShowsConnectionTrace(t *testing.T) {
	f := newFake(t)
	const password = "hunter2-very-secret"
	f.SetPassword(password)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c, _ := newTestClient(t, f, func(cfg *Config) {
		cfg.Logger = logger
		cfg.Password = password
	})
	ctx := context.Background()
	for range 2 {
		if _, err := c.Status(ctx); err != nil {
			t.Fatal(err)
		}
	}

	var requests []map[string]any
	for _, rec := range logRecords(t, &logs) {
		if rec["msg"] == "paprika request" {
			requests = append(requests, rec)
		}
	}
	// The login and two reads make three requests on one connection.
	if len(requests) != 3 {
		t.Fatalf("debug request lines = %d, want 3:\n%s", len(requests), logs.String())
	}
	wantReused := []bool{false, true, true}
	for i, rec := range requests {
		if rec["reused"] != wantReused[i] {
			t.Errorf("request %d: reused = %v, want %v", i+1, rec["reused"], wantReused[i])
		}
		if rec["conn_requests"] != float64(i+1) {
			t.Errorf("request %d: conn_requests = %v, want %d", i+1, rec["conn_requests"], i+1)
		}
		if rec["proto"] != "HTTP/1.1" {
			t.Errorf("request %d: proto = %v, want HTTP/1.1", i+1, rec["proto"])
		}
		for _, name := range []string{"was_idle", "idle_time"} {
			if _, ok := rec[name]; !ok {
				t.Errorf("request %d has no %s", i+1, name)
			}
		}
	}
	for _, secret := range []string{password, f.Email(), "Bearer", "Authorization", "fake-token-"} {
		if strings.Contains(logs.String(), secret) {
			t.Errorf("logs contain %q:\n%s", secret, logs.String())
		}
	}
}

func TestTraceReportsNegotiatedHTTP2(t *testing.T) {
	handler := func(w http.ResponseWriter, _ *http.Request) {
		_, _ = w.Write([]byte(`{"result":{"token":"t"}}`))
	}
	srv := httptest.NewUnstartedServer(http.HandlerFunc(handler))
	srv.EnableHTTP2 = true
	srv.StartTLS()
	t.Cleanup(srv.Close)

	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, &slog.HandlerOptions{Level: slog.LevelDebug}))
	c, err := New(Config{
		BaseURL:      srv.URL,
		Email:        "e",
		Password:     "p",
		HTTPClient:   srv.Client(),
		Logger:       logger,
		RequestLimit: -1,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := c.ensureToken(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, rec := range logRecords(t, &logs) {
		if rec["msg"] == "paprika request" {
			if rec["proto"] != "HTTP/2.0" {
				t.Fatalf("proto = %v, want HTTP/2.0", rec["proto"])
			}
			return
		}
	}
	t.Fatalf("no request line in the logs:\n%s", logs.String())
}

func TestConnCountsStayBounded(t *testing.T) {
	counts := newConnCounts()
	for range 3 * maxTrackedConns {
		counts.next(&fakeConn{})
	}
	if got := len(counts.counts); got > maxTrackedConns {
		t.Fatalf("tracked %d connections, bound is %d", got, maxTrackedConns)
	}
}

// fakeConn is a distinct net.Conn value to key the counters by.
type fakeConn struct{ net.Conn }
