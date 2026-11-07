package paprikaclient

import (
	"context"
	"sync"
	"time"
)

// Defaults for the request limiter. Paprika publishes no limit. The owner's
// live runs suggest it holds a request when more than 10 start inside a
// sliding window of about 11 s (see docs/paprika-api/notes/open-questions.md).
// The window here is slightly wider than that inferred 11 s for margin. Ten
// per 12 s sustains 50 a minute, the best rate observed to work. Up to 10
// requests may start at once after an idle spell, because 10 rapid requests
// were observed to pass. The defaults are tuned from observed stalls, never by
// probing for the limit.
const (
	DefaultRequestLimit  = 10
	DefaultRequestWindow = 12 * time.Second
	// BackgroundReserve is the number of request slots a background request
	// leaves free: it starts only if the count after it stays within the limit
	// minus this reserve, so tool calls are not stuck behind a long recipe load.
	BackgroundReserve = 2
	// DefaultStallCooldown is how long the next request waits after a timeout.
	DefaultStallCooldown = time.Minute
)

// limiterClock supplies time to a limiter. Tests replace it.
type limiterClock struct {
	now   func() time.Time
	sleep func(ctx context.Context, d time.Duration) error
}

func realSleep(ctx context.Context, d time.Duration) error {
	timer := time.NewTimer(d)
	defer timer.Stop()
	select {
	case <-timer.C:
		return nil
	case <-ctx.Done():
		return ctx.Err()
	}
}

// requestLogCap bounds the recorded start times when limiting is off.
const requestLogCap = 4096

// limiter is an in-memory sliding-window limiter: at most limit requests may
// start within any trailing window. It also remembers the start times so the
// stall warning can report the count. A limit of zero or less records without
// ever blocking.
type limiter struct {
	clock  limiterClock
	limit  int
	window time.Duration

	mu        sync.Mutex
	times     []time.Time // start times, oldest first
	holdUntil time.Time   // nothing may start before this
}

func newLimiter(limit int, window time.Duration, clock limiterClock) *limiter {
	if clock.now == nil {
		clock.now = time.Now
	}
	if clock.sleep == nil {
		clock.sleep = realSleep
	}
	return &limiter{clock: clock, limit: limit, window: window}
}

func (l *limiter) pruneLocked(now time.Time) {
	cut := 0
	for cut < len(l.times) && now.Sub(l.times[cut]) >= l.window {
		cut++
	}
	l.times = l.times[cut:]
}

// Wait blocks until one more request may start while keeping the number of
// starts in the trailing window within limit minus reserve (reserve 0 for
// interactive requests), then records the start and returns its time. It
// returns the context's error if ctx ends first.
func (l *limiter) Wait(ctx context.Context, reserve int) (time.Time, error) {
	for {
		if err := ctx.Err(); err != nil {
			return time.Time{}, err
		}
		l.mu.Lock()
		now := l.clock.now()
		var wait time.Duration
		if now.Before(l.holdUntil) {
			wait = l.holdUntil.Sub(now)
		} else {
			l.pruneLocked(now)
			allowed := l.limit - reserve
			if l.limit <= 0 || len(l.times) < allowed {
				l.record(now)
				l.mu.Unlock()
				return now, nil
			}
			// The request fits once enough of the oldest starts have left.
			ready := l.times[len(l.times)-max(allowed, 1)].Add(l.window)
			wait = ready.Sub(now)
		}
		l.mu.Unlock()
		if err := l.clock.sleep(ctx, max(wait, time.Millisecond)); err != nil {
			return time.Time{}, err
		}
	}
}

func (l *limiter) record(now time.Time) {
	if len(l.times) >= requestLogCap {
		l.times = l.times[1:]
	}
	l.times = append(l.times, now)
}

// Restamp moves a start recorded at old to now, for a request that waited
// behind the in-flight gate after Wait, so the window counts when it really
// started.
func (l *limiter) Restamp(old time.Time) {
	l.mu.Lock()
	defer l.mu.Unlock()
	now := l.clock.now()
	if !now.After(old) {
		return
	}
	for i, t := range l.times {
		if t.Equal(old) {
			l.times = append(l.times[:i], l.times[i+1:]...)
			l.times = append(l.times, now)
			return
		}
	}
}

// Hold keeps the limiter closed until cooldown has passed, so the next request
// waits cooldown before it starts.
func (l *limiter) Hold(cooldown time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.holdUntil = l.clock.now().Add(cooldown)
}

// Count returns the number of starts recorded in the window before now.
func (l *limiter) Count(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	return len(l.times)
}
