package middleware

import (
	"bytes"
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"
)

func discardLogger() *slog.Logger {
	return slog.New(slog.NewTextHandler(io.Discard, nil))
}

func ok() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	})
}

func TestClientIP(t *testing.T) {
	tests := []struct {
		name       string
		forwarded  string
		remoteAddr string
		want       string
	}{
		{name: "remote addr only", remoteAddr: "192.0.2.10:5555", want: "192.0.2.10"},
		{
			name:       "rightmost forwarded hop",
			forwarded:  "10.0.0.1, 203.0.113.7",
			remoteAddr: "10.0.0.2:1",
			want:       "203.0.113.7",
		},
		{
			name:       "spoofed left hops are ignored",
			forwarded:  "1.2.3.4, 5.6.7.8, 203.0.113.7",
			remoteAddr: "10.0.0.2:1",
			want:       "203.0.113.7",
		},
		{
			name:       "garbage last hop falls back",
			forwarded:  "203.0.113.7, not-an-ip",
			remoteAddr: "10.0.0.2:1",
			want:       "10.0.0.2",
		},
		{
			name:       "trailing comma falls back",
			forwarded:  "203.0.113.7,",
			remoteAddr: "10.0.0.2:1",
			want:       "10.0.0.2",
		},
		{
			name:       "single forwarded value",
			forwarded:  " 203.0.113.7 ",
			remoteAddr: "10.0.0.2:1",
			want:       "203.0.113.7",
		},
		{
			name:       "ipv6 forwarded",
			forwarded:  "2001:db8::1",
			remoteAddr: "10.0.0.2:1",
			want:       "2001:db8::1",
		},
		{
			name:       "garbage forwarded falls back",
			forwarded:  "not-an-ip",
			remoteAddr: "10.0.0.2:1",
			want:       "10.0.0.2",
		},
		{name: "remote addr without port", remoteAddr: "10.0.0.2", want: "10.0.0.2"},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			r.RemoteAddr = tt.remoteAddr
			if tt.forwarded != "" {
				r.Header.Set("X-Forwarded-For", tt.forwarded)
			}

			if got := ClientIP(r); got != tt.want {
				t.Errorf("ClientIP() = %q, want %q", got, tt.want)
			}
		})
	}
}

func TestOrigin(t *testing.T) {
	tests := []struct {
		name    string
		origin  string
		allowed []string
		want    int
	}{
		{name: "no origin", want: http.StatusOK},
		{name: "unknown origin", origin: "https://evil.example", want: http.StatusForbidden},
		{
			name:    "unknown origin with allow list",
			origin:  "https://evil.example",
			allowed: []string{"https://claude.ai"},
			want:    http.StatusForbidden,
		},
		{
			name:    "allowed origin",
			origin:  "https://claude.ai",
			allowed: []string{"https://claude.ai"},
			want:    http.StatusOK,
		},
		{
			name:    "allowed origin differs in case",
			origin:  "https://Claude.ai",
			allowed: []string{"https://claude.ai"},
			want:    http.StatusOK,
		},
		{name: "null origin", origin: "null", want: http.StatusForbidden},
		{
			name:    "different port",
			origin:  "http://localhost:9",
			allowed: []string{"http://localhost:3000"},
			want:    http.StatusForbidden,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := Origin(discardLogger(), tt.allowed)(ok())
			r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
			if tt.origin != "" {
				r.Header.Set("Origin", tt.origin)
			}
			w := httptest.NewRecorder()

			h.ServeHTTP(w, r)

			if w.Code != tt.want {
				t.Errorf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}
}

func TestBodyLimit(t *testing.T) {
	const limit = 16

	tests := []struct {
		name string
		body string
		want int
	}{
		{name: "under limit", body: strings.Repeat("a", limit), want: http.StatusOK},
		{
			name: "over limit by declared length",
			body: strings.Repeat("a", limit+1),
			want: http.StatusRequestEntityTooLarge,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			h := BodyLimit(limit)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if _, err := io.ReadAll(r.Body); err != nil {
					http.Error(w, "too large", http.StatusRequestEntityTooLarge)
				}
			}))
			w := httptest.NewRecorder()

			h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/mcp", strings.NewReader(tt.body)))

			if w.Code != tt.want {
				t.Errorf("status = %d, want %d", w.Code, tt.want)
			}
		})
	}

	t.Run("over limit without declared length", func(t *testing.T) {
		h := BodyLimit(limit)(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if _, err := io.ReadAll(r.Body); err != nil {
				http.Error(w, "too large", http.StatusRequestEntityTooLarge)
			}
		}))
		r := httptest.NewRequest(
			http.MethodPost,
			"/mcp",
			io.NopCloser(strings.NewReader(strings.Repeat("a", limit+1))),
		)
		r.ContentLength = -1
		w := httptest.NewRecorder()

		h.ServeHTTP(w, r)

		if w.Code != http.StatusRequestEntityTooLarge {
			t.Errorf("status = %d, want %d", w.Code, http.StatusRequestEntityTooLarge)
		}
	})
}

func TestTimeout(t *testing.T) {
	var deadlineSet bool
	h := Timeout(
		time.Minute,
		time.Second,
	)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		_, deadlineSet = r.Context().Deadline()
	}))

	h.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/mcp", nil))

	if !deadlineSet {
		t.Error("request context has no deadline")
	}

	expired := Timeout(
		time.Millisecond,
		time.Second,
	)(http.HandlerFunc(func(_ http.ResponseWriter, r *http.Request) {
		select {
		case <-r.Context().Done():
			if r.Context().Err() != context.DeadlineExceeded {
				t.Errorf("context error = %v, want deadline exceeded", r.Context().Err())
			}
		case <-time.After(5 * time.Second):
			t.Error("context was not cancelled")
		}
	}))
	expired.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/mcp", nil))
}

func TestTimeoutFallbackWhenHandlerOverruns(t *testing.T) {
	release := make(chan struct{})
	defer close(release)

	h := Timeout(
		20*time.Millisecond,
		20*time.Millisecond,
	)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		<-release // ignores its deadline
		_, _ = w.Write([]byte("too late"))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodPost, "/mcp", nil))

	if rec.Code != http.StatusServiceUnavailable {
		t.Errorf("status = %d, want 503", rec.Code)
	}
	if got := rec.Header().Get("Content-Type"); got != "application/json" {
		t.Errorf("Content-Type = %q", got)
	}
	body := rec.Body.String()
	if !strings.Contains(
		body,
		`"jsonrpc":"2.0"`,
	) || !strings.Contains(body, `"error"`) || strings.Contains(body, "too late") {
		t.Errorf("body = %q, want only the JSON-RPC error", body)
	}
}

func TestTimeoutPassesAnInTimeResponseThrough(t *testing.T) {
	h := Timeout(
		time.Minute,
		time.Second,
	)(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Content-Type", "text/plain")
		w.WriteHeader(http.StatusAccepted)
		_, _ = w.Write([]byte("fine"))
	}))

	rec := httptest.NewRecorder()
	h.ServeHTTP(rec, httptest.NewRequest(http.MethodGet, "/mcp", nil))

	if rec.Code != http.StatusAccepted || rec.Body.String() != "fine" || rec.Header().Get("Content-Type") != "text/plain" {
		t.Errorf("got %d %q %q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
}

func TestFailedAuthLimit(t *testing.T) {
	now := time.Unix(1000, 0)
	limiter := NewFailureLimiter(3, time.Minute)
	limiter.now = func() time.Time { return now }

	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))

	denyAll := http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		http.Error(w, "invalid token", http.StatusUnauthorized)
	})
	h := FailedAuthLimit(logger, limiter)(denyAll)

	send := func(ip string) int {
		r := httptest.NewRequest(http.MethodPost, "/mcp", nil)
		// A client can put anything to the left; the load balancer appends the real address.
		r.Header.Set("X-Forwarded-For", "9.9.9.9, "+ip)
		r.Header.Set("Authorization", "Bearer super-secret-value")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		return w.Code
	}

	for i := range 3 {
		if got := send("203.0.113.1"); got != http.StatusUnauthorized {
			t.Fatalf("failure %d: status = %d, want 401", i+1, got)
		}
	}
	if got := send("203.0.113.1"); got != http.StatusTooManyRequests {
		t.Errorf("after limit: status = %d, want 429 despite a spoofed left hop", got)
	}
	if got := send("203.0.113.2"); got != http.StatusUnauthorized {
		t.Errorf("other client: status = %d, want 401 (limit is per IP)", got)
	}

	now = now.Add(time.Minute + time.Second)
	if got := send("203.0.113.1"); got != http.StatusUnauthorized {
		t.Errorf("after window: status = %d, want 401", got)
	}

	logged := logs.String()
	for _, want := range []string{
		"authentication failed",
		"client_ip=203.0.113.1",
		"invalid token",
	} {
		if !strings.Contains(logged, want) {
			t.Errorf("log output missing %q: %s", want, logged)
		}
	}
	if strings.Contains(
		logged,
		"super-secret-value",
	) || strings.Contains(strings.ToLower(logged), "bearer") {
		t.Errorf("log output leaks the Authorization header: %s", logged)
	}
}

func TestFailedAuthLimitIgnoresSuccess(t *testing.T) {
	limiter := NewFailureLimiter(1, time.Minute)
	h := FailedAuthLimit(discardLogger(), limiter)(ok())

	for range 5 {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/mcp", nil))
		if w.Code != http.StatusOK {
			t.Fatalf("status = %d, want 200", w.Code)
		}
	}
}

func TestFailureLimiterBoundsMemory(t *testing.T) {
	limiter := NewFailureLimiter(3, time.Minute)
	limiter.maxEntries = 5

	for i := range 50 {
		limiter.RecordFailure("10.0.0." + string(rune('a'+i%26)) + string(rune('a'+i/26)))
	}

	if len(limiter.entries) > limiter.maxEntries {
		t.Errorf("entries = %d, want at most %d", len(limiter.entries), limiter.maxEntries)
	}
}

func TestGlobalLimit(t *testing.T) {
	now := time.Unix(1000, 0)
	bucket := NewTokenBucket(1, 2)
	bucket.now = func() time.Time { return now }
	bucket.last = now

	h := GlobalLimit(bucket)(ok())
	send := func() int {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest(http.MethodPost, "/mcp", nil))
		return w.Code
	}

	if send() != http.StatusOK || send() != http.StatusOK {
		t.Fatal("burst of 2 should be allowed")
	}
	if got := send(); got != http.StatusTooManyRequests {
		t.Errorf("over burst: status = %d, want 429", got)
	}

	now = now.Add(1500 * time.Millisecond)
	if got := send(); got != http.StatusOK {
		t.Errorf("after refill: status = %d, want 200", got)
	}
	if got := send(); got != http.StatusTooManyRequests {
		t.Errorf("refill is only one token: status = %d, want 429", got)
	}
}
