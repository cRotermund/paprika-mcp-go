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

// limitedClient returns a client with the token bucket on (the defaults: 50 a
// minute, burst 4) and a fake clock that sleeping advances.
func limitedClient(
	t *testing.T,
	f *fakepaprika.Server,
	mutate func(*Config),
) (*Client, *stepClock) {
	t.Helper()
	sc := newStepClock()
	c, _ := newTestClient(t, f, func(cfg *Config) {
		cfg.RequestsPerMinute = DefaultRequestsPerMinute
		cfg.RequestBurst = DefaultRequestBurst
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
		{"burst equal to the reserve", 50, BackgroundReserve, true},
		{"burst below the reserve", 50, 1, true},
		{"burst above the reserve", 50, BackgroundReserve + 1, false},
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

func TestDefaultsAreFiftyPerMinuteWithBurstFour(t *testing.T) {
	if DefaultRequestsPerMinute != 50 || DefaultRequestBurst != 4 {
		t.Fatalf(
			"defaults = %d/%d, want 50/4",
			DefaultRequestsPerMinute,
			DefaultRequestBurst,
		)
	}
	c, err := New(Config{Email: "e", Password: "p"})
	if err != nil {
		t.Fatal(err)
	}
	if c.cfg.RequestsPerMinute != 50 || c.cfg.RequestBurst != 4 || c.bucket == nil {
		t.Fatalf(
			"client limits = %d/%d, want 50/4 with a bucket",
			c.cfg.RequestsPerMinute,
			c.cfg.RequestBurst,
		)
	}
}

func TestLoginsAndRequestsShareTheBurst(t *testing.T) {
	f := newFake(t)
	c, sc := limitedClient(t, f, nil)
	// One login and nine reads make ten requests against a burst of four, so
	// six wait one interval (1.2 s at 50 a minute) each.
	for range 9 {
		if _, err := c.Status(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	if got := len(f.Requests()); got != 10 {
		t.Fatalf("requests = %d, want 10", got)
	}
	if got := sc.total(); got != 6*1200*time.Millisecond {
		t.Fatalf("waited %s, want 7.2s for the six requests past the burst", got)
	}
}

func TestBackgroundStopsAtTheReserve(t *testing.T) {
	f := newFake(t)
	c, sc := limitedClient(t, f, nil)
	bg := Background(context.Background())

	// The login takes 1 of 4 tokens; a background request may take a token
	// only while the reserve of 2 would remain, which allows one more.
	if _, err := c.Status(bg); err != nil {
		t.Fatalf("background request: %v", err)
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

	c.bucket.clock = sc.clock()
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

// stallRecord returns the stall warning from the logs.
func stallRecord(t *testing.T, logs *bytes.Buffer) map[string]any {
	t.Helper()
	for _, rec := range logRecords(t, logs) {
		if strings.Contains(rec["msg"].(string), "stalled") {
			return rec
		}
	}
	t.Fatalf("no stall warning in the logs:\n%s", logs.String())
	return nil
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

	stall := stallRecord(t, &logs)
	want := map[string]any{
		"level":               "WARN",
		"requests_last_12s":   float64(2),
		"requests_per_minute": float64(DefaultRequestsPerMinute),
		"burst":               float64(DefaultRequestBurst),
		"method":              "GET",
		"path":                statusPath,
		"cooldown":            float64(90 * time.Second),
		"reused":              true,
		"conn_requests":       float64(2),
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

// clockJump is a transport that moves the fake clock forward when a request
// for path is sent, standing in for the 20 s a stalled request spends waiting.
type clockJump struct {
	sc    *stepClock
	armed *bool
	path  string
	by    time.Duration
}

func (j clockJump) RoundTrip(req *http.Request) (*http.Response, error) {
	if *j.armed && strings.HasSuffix(req.URL.Path, j.path) {
		j.sc.now = j.sc.now.Add(j.by)
	}
	return http.DefaultTransport.RoundTrip(req)
}

// TestStallCountIsTakenWhenTheRequestStarts checks requests_last_12s: it counts
// the requests that started in the 12 s up to and including the stalled one,
// measured when it started. A login and four reads, then a stalled read, give
// 6. The clock jumps 20 s while the stalled request waits, so a count taken
// when the timeout fires would show 0 or 1.
func TestStallCountIsTakenWhenTheRequestStarts(t *testing.T) {
	f := newFake(t)
	var logs bytes.Buffer
	logger := slog.New(slog.NewJSONHandler(&logs, nil))
	sc := newStepClock()
	armed := false
	c, _ := newTestClient(t, f, func(cfg *Config) {
		cfg.Logger = logger
		cfg.RequestsPerMinute = 600
		cfg.RequestBurst = 8
		cfg.RequestTimeout = 30 * time.Millisecond
		cfg.Now = func() time.Time { return sc.now }
		cfg.HTTPClient = &http.Client{
			Transport: clockJump{sc: sc, armed: &armed, path: statusPath, by: 20 * time.Second},
		}
	})
	c.bucket.clock = sc.clock()
	ctx := context.Background()
	for range 4 {
		if _, err := c.Status(ctx); err != nil {
			t.Fatal(err)
		}
	}

	f.Inject(fakepaprika.Fault{
		Path:  statusPath,
		Kind:  fakepaprika.FaultDelay,
		Delay: 300 * time.Millisecond,
	})
	armed = true
	if _, err := c.Status(ctx); !errors.Is(err, ErrTemporary) {
		t.Fatalf("error = %v, want ErrTemporary", err)
	}

	stall := stallRecord(t, &logs)
	if stall["requests_last_12s"] != float64(6) {
		t.Errorf("requests_last_12s = %v, want 6", stall["requests_last_12s"])
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
		BaseURL:           srv.URL,
		Email:             "e",
		Password:          "p",
		HTTPClient:        srv.Client(),
		Logger:            logger,
		RequestsPerMinute: -1,
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
