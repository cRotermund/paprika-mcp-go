package middleware

import (
	"sync"
	"time"
)

// FailureLimiter counts failed authentications per client IP in a fixed
// window. Once an IP reaches the limit it is blocked until its window ends.
type FailureLimiter struct {
	mu         sync.Mutex
	max        int
	window     time.Duration
	maxEntries int
	now        func() time.Time
	entries    map[string]*failureEntry
}

type failureEntry struct {
	count   int
	resetAt time.Time
}

// NewFailureLimiter blocks an IP after maxFailures failures within window.
func NewFailureLimiter(maxFailures int, window time.Duration) *FailureLimiter {
	return &FailureLimiter{
		max:        maxFailures,
		window:     window,
		maxEntries: 10000,
		now:        time.Now,
		entries:    make(map[string]*failureEntry),
	}
}

// Blocked reports whether ip is over the limit and how long until it clears.
func (l *FailureLimiter) Blocked(ip string) (bool, time.Duration) {
	l.mu.Lock()
	defer l.mu.Unlock()

	e, ok := l.entries[ip]
	if !ok {
		return false, 0
	}

	now := l.now()
	if !now.Before(e.resetAt) {
		delete(l.entries, ip)
		return false, 0
	}

	return e.count >= l.max, e.resetAt.Sub(now)
}

// RecordFailure counts one failed authentication for ip.
func (l *FailureLimiter) RecordFailure(ip string) {
	l.mu.Lock()
	defer l.mu.Unlock()

	now := l.now()
	if e, ok := l.entries[ip]; ok && now.Before(e.resetAt) {
		e.count++
		return
	}

	if len(l.entries) >= l.maxEntries {
		l.evict(now)
	}
	l.entries[ip] = &failureEntry{count: 1, resetAt: now.Add(l.window)}
}

// evict drops expired entries, then arbitrary ones if the map is still full,
// so memory stays bounded however many addresses probe the server.
func (l *FailureLimiter) evict(now time.Time) {
	for ip, e := range l.entries {
		if !now.Before(e.resetAt) {
			delete(l.entries, ip)
		}
	}
	for ip := range l.entries {
		if len(l.entries) < l.maxEntries {
			return
		}
		delete(l.entries, ip)
	}
}

// TokenBucket is a global rate limiter: it refills at a fixed rate up to a
// burst size, and each allowed request takes one token.
type TokenBucket struct {
	mu     sync.Mutex
	rate   float64
	burst  float64
	tokens float64
	last   time.Time
	now    func() time.Time
}

// NewTokenBucket returns a full bucket.
func NewTokenBucket(ratePerSecond float64, burst int) *TokenBucket {
	return &TokenBucket{
		rate:   ratePerSecond,
		burst:  float64(burst),
		tokens: float64(burst),
		last:   time.Now(),
		now:    time.Now,
	}
}

// Allow takes a token if one is available. When none is, the second result
// estimates how long until one is.
func (b *TokenBucket) Allow() (bool, time.Duration) {
	b.mu.Lock()
	defer b.mu.Unlock()

	now := b.now()
	b.tokens = min(b.burst, b.tokens+now.Sub(b.last).Seconds()*b.rate)
	b.last = now

	if b.tokens >= 1 {
		b.tokens--
		return true, 0
	}

	return false, time.Duration((1 - b.tokens) / b.rate * float64(time.Second))
}
