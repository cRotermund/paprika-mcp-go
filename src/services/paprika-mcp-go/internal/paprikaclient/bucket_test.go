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

func (s *stepClock) clock() bucketClock {
	return bucketClock{
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

func TestBucketBurstThenRate(t *testing.T) {
	tests := []struct {
		name      string
		perMinute int
		burst     int
		waits     int
		wantSlept time.Duration
		wantSleep int
	}{
		{"burst is free", 60, 3, 3, 0, 0},
		{"one past the burst waits one interval", 60, 3, 4, time.Second, 1},
		{"sustained rate", 60, 3, 8, 5 * time.Second, 5},
		{"faster rate", 120, 2, 6, 2 * time.Second, 4},
		{"slower rate", 30, 1, 3, 4 * time.Second, 2},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := newStepClock()
			b := newBucket(tt.perMinute, tt.burst, sc.clock())
			for range tt.waits {
				if err := b.Wait(context.Background(), 0); err != nil {
					t.Fatal(err)
				}
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

func TestBucketRefillIsCappedAtBurst(t *testing.T) {
	sc := newStepClock()
	b := newBucket(60, 3, sc.clock())
	sc.now = sc.now.Add(time.Hour)
	for range 3 {
		if err := b.Wait(context.Background(), 0); err != nil {
			t.Fatal(err)
		}
	}
	if len(sc.slept) != 0 {
		t.Fatalf("slept %v within the burst", sc.slept)
	}
	if err := b.Wait(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if got := sc.total(); got != time.Second {
		t.Fatalf("slept %s after the burst, want 1s", got)
	}
}

func TestBucketWaitHonorsContext(t *testing.T) {
	sc := newStepClock()
	b := newBucket(60, 1, sc.clock())
	if err := b.Wait(context.Background(), 0); err != nil {
		t.Fatal(err)
	}

	ctx, cancel := context.WithCancel(context.Background())
	clock := sc.clock()
	clock.sleep = func(ctx context.Context, _ time.Duration) error {
		cancel()
		return ctx.Err()
	}
	b.clock = clock
	if err := b.Wait(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("error = %v, want context canceled", err)
	}

	if err := b.Wait(ctx, 0); !errors.Is(err, context.Canceled) {
		t.Fatalf("an already canceled context: error = %v, want canceled", err)
	}
}

func TestBucketReserve(t *testing.T) {
	tests := []struct {
		name    string
		reserve int
		taken   int
		wantNow bool
	}{
		{"interactive with one left", 0, 4, true},
		{"background with the reserve left blocks", BackgroundReserve, 3, false},
		{"background just above the reserve proceeds", BackgroundReserve, 2, true},
		{"interactive with only the reserve left", 0, 3, true},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			sc := newStepClock()
			b := newBucket(60, 5, sc.clock())
			for range tt.taken {
				if err := b.Wait(context.Background(), 0); err != nil {
					t.Fatal(err)
				}
			}
			before := len(sc.slept)
			if err := b.Wait(context.Background(), tt.reserve); err != nil {
				t.Fatal(err)
			}
			gotNow := len(sc.slept) == before
			if gotNow != tt.wantNow {
				t.Fatalf("immediate = %v, want %v (slept %v)", gotNow, tt.wantNow, sc.slept)
			}
		})
	}
}

func TestBucketDrainHoldsForCooldown(t *testing.T) {
	sc := newStepClock()
	b := newBucket(60, 8, sc.clock())
	b.Drain(time.Minute)
	if err := b.Wait(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if got := sc.total(); got != time.Minute {
		t.Fatalf("waited %s after a drain, want 1m", got)
	}
	if err := b.Wait(context.Background(), 0); err != nil {
		t.Fatal(err)
	}
	if got := sc.total(); got != time.Minute+time.Second {
		t.Fatalf("waited %s, want the next token one interval later", got)
	}
}

func TestRequestLogCountsOnlyTheWindow(t *testing.T) {
	start := time.Date(2030, 1, 1, 0, 0, 0, 0, time.UTC)
	l := newRequestLog(time.Minute)
	offsets := []time.Duration{
		0,
		10 * time.Second,
		59 * time.Second,
		61 * time.Second,
	}
	for _, offset := range offsets {
		l.Add(start.Add(offset))
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
