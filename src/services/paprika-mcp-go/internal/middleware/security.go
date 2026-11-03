package middleware

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"slices"
	"strconv"
	"strings"
	"time"
)

const maxReasonBytes = 128

// ClientIP returns the rightmost hop of X-Forwarded-For, or the host part of
// RemoteAddr when the header is absent or its last hop is not an IP address.
// The load balancer appends the address it saw to the end of the header, so the
// rightmost hop is the one a client cannot forge; earlier hops are whatever the
// client chose to send. The header is trusted only because the service is
// reachable solely through the load balancer.
func ClientIP(r *http.Request) string {
	if values := r.Header.Values("X-Forwarded-For"); len(values) > 0 {
		last := values[len(values)-1]
		if i := strings.LastIndexByte(last, ','); i >= 0 {
			last = last[i+1:]
		}
		if ip := net.ParseIP(strings.TrimSpace(last)); ip != nil {
			return ip.String()
		}
	}

	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return r.RemoteAddr
	}

	return host
}

// FailedAuthLimit wraps the authentication middleware. It refuses clients that
// have failed too often, and counts and logs each new failure (a 401 or 403
// from the wrapped handler). The log line carries the client IP and the
// reason, never the Authorization header.
func FailedAuthLimit(logger *slog.Logger, limiter *FailureLimiter) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ip := ClientIP(r)

			if blocked, retryAfter := limiter.Blocked(ip); blocked {
				logger.Debug("authentication blocked", "client_ip", ip)
				tooManyRequests(w, retryAfter)
				return
			}

			rec := &statusRecorder{ResponseWriter: w}
			next.ServeHTTP(rec, r)

			if rec.status == http.StatusUnauthorized || rec.status == http.StatusForbidden {
				limiter.RecordFailure(ip)
				logger.Warn("authentication failed",
					"client_ip", ip,
					"reason", strings.TrimSpace(rec.reason.String()))
			}
		})
	}
}

// GlobalLimit rejects requests beyond the bucket's rate. Place it after
// authentication so that it only counts authenticated traffic.
func GlobalLimit(bucket *TokenBucket) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if ok, retryAfter := bucket.Allow(); !ok {
				tooManyRequests(w, retryAfter)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

// Origin rejects requests whose Origin header is present and not in allowed.
// Requests without one, which is normal for server-side clients, pass.
func Origin(logger *slog.Logger, allowed []string) func(http.Handler) http.Handler {
	normalized := make([]string, 0, len(allowed))
	for _, o := range allowed {
		normalized = append(normalized, normalizeOrigin(o))
	}

	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			origin := r.Header.Get("Origin")
			if origin != "" && !slices.Contains(normalized, normalizeOrigin(origin)) {
				logger.Warn("origin rejected", "client_ip", ClientIP(r), "origin", origin)
				http.Error(w, "origin not allowed", http.StatusForbidden)
				return
			}
			next.ServeHTTP(w, r)
		})
	}
}

func normalizeOrigin(o string) string {
	return strings.ToLower(strings.TrimRight(o, "/"))
}

// BodyLimit rejects request bodies larger than limit bytes.
func BodyLimit(limit int64) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if r.ContentLength > limit {
				http.Error(w, "request body too large", http.StatusRequestEntityTooLarge)
				return
			}
			r.Body = http.MaxBytesReader(w, r.Body, limit)
			next.ServeHTTP(w, r)
		})
	}
}

// timeoutBody is the JSON-RPC error sent when a handler overruns. Its id is
// null because the middleware does not parse the request.
const timeoutBody = `{"jsonrpc":"2.0","id":null,"error":{"code":-32603,"message":"The server did not finish in time. If the request changed anything its outcome is unknown: read the data again before retrying."}}`

// Timeout cancels the request context after d, so handlers can end their work
// and write a proper error themselves. If the handler has not finished grace
// later, the caller gets HTTP 503 with a JSON-RPC error body instead of an
// empty response. The fallback is http.TimeoutHandler, which buffers the
// handler's writes so the two never write to the connection at once; a handler
// that answers in time is passed through unchanged.
func Timeout(d, grace time.Duration) func(http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		inner := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			ctx, cancel := context.WithTimeout(r.Context(), d)
			defer cancel()
			next.ServeHTTP(w, r.WithContext(ctx))
		})
		fallback := http.TimeoutHandler(inner, d+grace, timeoutBody)

		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			// TimeoutHandler sends its body without a content type. A handler's own
			// Content-Type replaces this default.
			w.Header().Set("Content-Type", "application/json")
			fallback.ServeHTTP(w, r)
		})
	}
}

func tooManyRequests(w http.ResponseWriter, retryAfter time.Duration) {
	seconds := max(1, int(retryAfter.Seconds())+1)
	w.Header().Set("Retry-After", strconv.Itoa(seconds))
	http.Error(w, "too many requests", http.StatusTooManyRequests)
}

// statusRecorder remembers the status code and, for an authentication
// failure, the short reason text the SDK middleware writes.
type statusRecorder struct {
	http.ResponseWriter
	status int
	reason strings.Builder
}

func (s *statusRecorder) WriteHeader(code int) {
	if s.status == 0 {
		s.status = code
	}
	s.ResponseWriter.WriteHeader(code)
}

func (s *statusRecorder) Write(b []byte) (int, error) {
	if s.status == 0 {
		s.status = http.StatusOK
	}
	if s.status == http.StatusUnauthorized || s.status == http.StatusForbidden {
		if room := maxReasonBytes - s.reason.Len(); room > 0 {
			s.reason.Write(b[:min(room, len(b))])
		}
	}

	return s.ResponseWriter.Write(b)
}

func (s *statusRecorder) Flush() {
	_ = http.NewResponseController(s.ResponseWriter).Flush()
}

func (s *statusRecorder) Unwrap() http.ResponseWriter {
	return s.ResponseWriter
}
