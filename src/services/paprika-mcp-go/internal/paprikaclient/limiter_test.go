package paprikaclient

import (
	"context"
	"errors"
	"testing"
	"time"
)

// stepClock is a clock whose sleeps advance it instead of waiting.
type stepClock struct {
	now   time.Time
	slept []time.Duration
}

func newStepClock() *stepClock {
	return &stepClock{now: time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)}
}

func (s *stepClock) clock() limiterClock {
	return limiterClock{
		now: func() time.Time { return s.now },
		sleep: func(ctx context.Context, d time.Duration) error {
			if err := ctx.Err(); err != nil {
				return err
			}
			s.slept = append(s.slept, d)
			s.now = s.now.Add(d)
			return nil
		},
	}
}

func (s *stepClock) total() time.Duration {
	var sum time.Duration
	for _, d := range s.slept {
		sum += d
	}
	return sum
}

func mustWait(t *testing.T, l *limiter, reserve int) {
	t.Helper()
	if _, err := l.Wait(context.Background(), reserve); err != nil {
		t.Fatal(err)
	}
}

func TestLimiterWindow(t *testing.T) {
	tests := []struct {
		name      string
		limit     int
		window    time.Duration
		waits     int
		wantSlept time.Duration
		wantSleep int
	}{
		{
			name:   "the limit goes at once",
			limit:  3,
			window: 10 * time.Second,
			waits:  3,
		},
		{
			name:      "one past the limit waits for the oldest",
			limit:     3,
			window:    10 * time.Second,
			waits:     4,
			wantSlept: 10 * time.Second,
			wantSleep: 1,
		},
		{
			name:      "a second batch goes together",
			limit:     3,
			window:    10 * time.Second,
			waits:     6,
			wantSlept: 10 * time.Second,
			wantSleep: 1,
		},
		{
			name:      "a third batch waits a second window",
			limit:     3,
			window:    10 * time.Second,
			waits:     7,
			wantSlept: 20 * time.Second,
			wantSleep: 2,
		},
		{
			name:      "the default window",
			limit:     DefaultRequestLimit,
			window:    DefaultRequestWindow,
			waits:     11,
			wantSlept: 12 * time.Second,
			wantSleep: 1,
		},
		{
			name:      "a limit of one",
			limit:     1,
			window:    time.Second,
			waits:     3,
			wantSlept: 2 * time.Second,
			wantSleep: 2,
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := newStepClock()
			l := newLimiter(tt.limit, tt.window, sc.clock())
			for range tt.waits {
				mustWait(t, l, 0)
			}
			if got := sc.total(); got != tt.wantSlept {
				t.Errorf("slept %s, want %s", got, tt.wantSlept)
			}
			if len(sc.slept) != tt.wantSleep {
				t.Errorf("sleeps = %d, want %d", len(sc.slept), tt.wantSleep)
			}
		})
	}
}

func TestLimiterNextWaitsUntilTheOldestLeaves(t *testing.T) {
	sc := newStepClock()
	l := newLimiter(3, 10*time.Second, sc.clock())
	for range 3 {
		mustWait(t, l, 0)
		sc.now = sc.now.Add(2 * time.Second) // starts at 0 s, 2 s, 4 s
	}
	// Now 6 s. The oldest start (0 s) leaves the window at 10 s.
	mustWait(t, l, 0)
	if got := sc.total(); got != 4*time.Second {
		t.Fatalf("waited %s, want 4s until the oldest start leaves", got)
	}
}

func TestLimiterSteadySpacingNeverWaits(t *testing.T) {
	tests := []struct {
		name    string
		spacing time.Duration
	}{
		{"exactly window over limit", DefaultRequestWindow / DefaultRequestLimit},
		{"slower than that", 1500 * time.Millisecond},
		{"much slower", 10 * time.Second},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := newStepClock()
			l := newLimiter(DefaultRequestLimit, DefaultRequestWindow, sc.clock())
			for range 200 {
				mustWait(t, l, 0)
				if got := l.Count(sc.now); got > DefaultRequestLimit {
					t.Fatalf("%d starts in the window, limit %d", got, DefaultRequestLimit)
				}
				sc.now = sc.now.Add(tt.spacing)
			}
			if len(sc.slept) != 0 {
				t.Fatalf("steady requests waited: %v", sc.slept)
			}
		})
	}
}

func TestLimiterNeverExceedsTheLimitInAnyWindow(t *testing.T) {
	sc := newStepClock()
	l := newLimiter(DefaultRequestLimit, DefaultRequestWindow, sc.clock())
	var starts []time.Time
	for range 120 {
		at, err := l.Wait(context.Background(), 0)
		if err != nil {
			t.Fatal(err)
		}
		starts = append(starts, at)
	}
	for i := DefaultRequestLimit; i < len(starts); i++ {
		if starts[i].Sub(starts[i-DefaultRequestLimit]) < DefaultRequestWindow {
			t.Fatalf("starts %d to %d fit in %s", i-DefaultRequestLimit, i, DefaultRequestWindow)
		}
	}
}

func TestLimiterReserve(t *testing.T) {
	tests := []struct {
		name    string
		reserve int
		taken   int
		wantNow bool
	}{
		{"interactive with the window nearly full", 0, 9, true},
		{"interactive at the reserve", 0, 8, true},
		{"background just under the reserve proceeds", BackgroundReserve, 7, true},
		{"background at the reserve waits", BackgroundReserve, 8, false},
		{"background above the reserve waits", BackgroundReserve, 9, false},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := newStepClock()
			l := newLimiter(DefaultRequestLimit, DefaultRequestWindow, sc.clock())
			for range tt.taken {
				mustWait(t, l, 0)
			}
			mustWait(t, l, tt.reserve)
			gotNow := len(sc.slept) == 0
			if gotNow != tt.wantNow {
				t.Fatalf("immediate = %v, want %v (slept %v)", gotNow, tt.wantNow, sc.slept)
			}
		})
	}
}

func TestLimiterBackgroundWaitsWhileInteractiveProceeds(t *testing.T) {
	sc := newStepClock()
	l := newLimiter(DefaultRequestLimit, DefaultRequestWindow, sc.clock())
	for range DefaultRequestLimit - BackgroundReserve {
		mustWait(t, l, BackgroundReserve)
	}

	ctx, cancel := context.WithCancel(context.Background())
	clock := sc.clock()
	clock.sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	l.clock = clock
	if _, err := l.Wait(ctx, BackgroundReserve); !errors.Is(err, context.Canceled) {
		t.Fatalf("background error = %v, want it held until canceled", err)
	}

	l.clock = sc.clock()
	for range BackgroundReserve {
		mustWait(t, l, 0)
	}
	if len(sc.slept) != 0 {
		t.Fatalf("interactive requests waited inside the reserve: %v", sc.slept)
	}
}

func TestLimiterWaitHonorsContext(t *testing.T) {
	sc := newStepClock()
	l := newLimiter(1, time.Minute, sc.clock())
	mustWait(t, l, 0)

	ctx, cancel := context.WithCancel(context.Background())
	clock := sc.clock()
	clock.sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	l.clock = clock
	if _, err := l.Wait(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}
	if _, err := l.Wait(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("an already canceled context: error = %v, want canceled", err)
	}
	if got := l.Count(sc.now); got != 1 {
		t.Fatalf("a canceled wait recorded a start: count = %d, want 1", got)
	}
}

func TestLimiterHoldDelaysTheNextStart(t *testing.T) {
	sc := newStepClock()
	l := newLimiter(DefaultRequestLimit, DefaultRequestWindow, sc.clock())
	mustWait(t, l, 0)
	l.Hold(time.Minute)

	mustWait(t, l, 0)
	if got := sc.total(); got != time.Minute {
		t.Fatalf("waited %s after a hold, want 1m", got)
	}
	mustWait(t, l, 0)
	if got := sc.total(); got != time.Minute {
		t.Fatalf("waited %s, want no further wait once the hold ended", got)
	}
}

func TestLimiterOffRecordsWithoutBlocking(t *testing.T) {
	sc := newStepClock()
	l := newLimiter(-1, time.Second, sc.clock())
	for range 50 {
		mustWait(t, l, BackgroundReserve)
	}
	if len(sc.slept) != 0 {
		t.Fatalf("a disabled limiter waited: %v", sc.slept)
	}
	if got := l.Count(sc.now); got != 50 {
		t.Fatalf("count = %d, want 50", got)
	}
}

func TestLimiterCountsOnlyTheWindow(t *testing.T) {
	sc := newStepClock()
	l := newLimiter(100, time.Minute, sc.clock())
	start := sc.now
	offsets := []time.Duration{
		0,
		10 * time.Second,
		59 * time.Second,
		61 * time.Second,
	}
	for _, offset := range offsets {
		sc.now = start.Add(offset)
		mustWait(t, l, 0)
	}
	tests := []struct {
		at   time.Duration
		want int
	}{
		{61 * time.Second, 3},
		{70 * time.Second, 2},
		{125 * time.Second, 0},
	}
	for _, tt := range tests {
		if got := l.Count(start.Add(tt.at)); got != tt.want {
			t.Errorf("count at %s = %d, want %d", tt.at, got, tt.want)
		}
	}
}

func TestLimiterRestampMovesAStartToItsRealTime(t *testing.T) {
	sc := newStepClock()
	l := newLimiter(2, 10*time.Second, sc.clock())
	first, err := l.Wait(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	mustWait(t, l, 0)

	sc.now = sc.now.Add(8 * time.Second) // the first request really started late
	l.Restamp(first)
	sc.now = sc.now.Add(3 * time.Second) // 11 s: the original stamp would have left

	if got := l.Count(sc.now); got != 1 {
		t.Fatalf("count = %d, want only the restamped start", got)
	}
}
