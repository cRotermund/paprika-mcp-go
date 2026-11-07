package paprikaclient

import (
	"context"
	"sync"
	"time"
)

// Defaults for the request limiter. Paprika publishes no limit. The owner's
// live runs found that evenly spaced requests pass at about 50 a minute, and
// that bursts after recent activity are poorly tolerated (see
// docs/paprika-api/notes/open-questions.md). 50 a minute with a burst of 4 is
// the setting that loaded a full library of 228 recipes on the real account.
// It sits near the observed edge, so 45 a minute with a burst of 4 is the
// fallback if stalls appear. Tune from observed stalls, never by probing for
// the limit.
const (
	DefaultRequestsPerMinute = 50
	DefaultRequestBurst      = 4
	// BackgroundReserve is the number of tokens a background request leaves in
	// the bucket: it takes a token only if at least this many would remain, so
	// tool calls are not stuck behind a long recipe load.
	BackgroundReserve = 2
	// DefaultStallCooldown is how long the next request waits after a timeout
	// emptied the bucket.
	DefaultStallCooldown = time.Minute
	// StallSpan is the span over which a stall warning counts the requests
	// that started up to and including the stalled one. 12 s is the span that
	// seems to matter in the live runs; the client does not enforce it.
	StallSpan = 12 * time.Second
)

// bucketClock supplies time to a bucket. Tests replace it.
type bucketClock struct {
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

// bucket is an in-memory token bucket. It holds up to burst tokens and gains
// one every interval. Tokens are fractional internally.
type bucket struct {
	clock    bucketClock
	interval time.Duration
	burst    float64

	mu     sync.Mutex
	tokens float64
	last   time.Time // time of the last refill; may lie in the future after drain
}

// newBucket returns a full bucket that refills perMinute tokens a minute.
func newBucket(perMinute, burst int, clock bucketClock) *bucket {
	if clock.now == nil {
		clock.now = time.Now
	}
	if clock.sleep == nil {
		clock.sleep = realSleep
	}
	return &bucket{
		clock:    clock,
		interval: time.Minute / time.Duration(perMinute),
		burst:    float64(burst),
		tokens:   float64(burst),
		last:     clock.now(),
	}
}

// refillLocked adds the tokens earned since the last refill.
func (b *bucket) refillLocked(now time.Time) {
	if !now.After(b.last) {
		return
	}
	b.tokens = min(b.burst, b.tokens+float64(now.Sub(b.last))/float64(b.interval))
	b.last = now
}

// Wait blocks until a token is available, then takes it. A token is available
// only when at least reserve whole tokens would remain afterwards (reserve 0
// for ordinary requests). It returns the context's error if ctx ends first.
func (b *bucket) Wait(ctx context.Context, reserve int) error {
	need := 1 + float64(reserve)
	for {
		if err := ctx.Err(); err != nil {
			return err
		}
		b.mu.Lock()
		now := b.clock.now()
		b.refillLocked(now)
		if b.tokens >= need {
			b.tokens--
			b.mu.Unlock()
			return nil
		}
		ready := b.last.Add(time.Duration((need - b.tokens) * float64(b.interval)))
		wait := max(ready.Sub(now), time.Millisecond)
		b.mu.Unlock()
		if err := b.clock.sleep(ctx, wait); err != nil {
			return err
		}
	}
}

// Drain empties the bucket and holds refilling until cooldown has passed, so
// the next interactive request waits cooldown and then gets its token.
func (b *bucket) Drain(cooldown time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.tokens = 0
	b.last = b.clock.now().Add(cooldown - b.interval)
}

// requestLog remembers when requests started, to report how many started in
// the last window. It never sends anything itself.
type requestLog struct {
	mu     sync.Mutex
	window time.Duration
	times  []time.Time
}

// requestLogCap bounds the log when limiting is off.
const requestLogCap = 4096

func newRequestLog(window time.Duration) *requestLog {
	return &requestLog{window: window}
}

// Add records a request start at now and returns how many requests, this one
// included, started within the window ending at now.
func (l *requestLog) Add(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	if len(l.times) >= requestLogCap {
		l.times = l.times[1:]
	}
	l.times = append(l.times, now)
	return len(l.times)
}

// Count returns the number of requests recorded within the window before now.
func (l *requestLog) Count(now time.Time) int {
	l.mu.Lock()
	defer l.mu.Unlock()
	l.pruneLocked(now)
	return len(l.times)
}

func (l *requestLog) pruneLocked(now time.Time) {
	cut := 0
	for cut < len(l.times) && now.Sub(l.times[cut]) >= l.window {
		cut++
	}
	l.times = l.times[cut:]
}
