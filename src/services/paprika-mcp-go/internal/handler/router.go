package handler

import (
	"log/slog"
	"net/http"
	"time"

	"github.com/go-chi/chi/v5"
	chimiddleware "github.com/go-chi/chi/v5/middleware"
	sdkauth "github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/middleware"
)

// Defaults for the request limits. One person uses this server, so the global
// limit is generous for a chatty client yet far below what could hammer
// Paprika. Failed authentication is limited much harder: a legitimate client
// fails rarely, and a guesser needs many attempts.
//
// DefaultRequestTimeout is the budget for one inbound request, and it must be
// several times paprikaclient.DefaultRequestTimeout (20 s). The Paprika client
// serves one call at a time, so a tool call can queue behind other callers'
// hung calls (three hangs in a row make the client pause), and a write makes
// several calls in sequence. The worst plausible write is two hung calls ahead
// of it (40 s), then its own hung POST (20 s): 60 s, plus pacing and healthy
// calls. 90 s covers that with margin, so a Paprika hang always surfaces as the
// tool's own "temporarily unavailable" or "outcome unknown" result before the
// request deadline.
//
// DefaultTimeoutGrace is how long after that deadline the handler may still
// finish writing its response before the fallback 503 is sent.
const (
	DefaultMaxBodyBytes     = 1 << 20
	DefaultRequestTimeout   = 90 * time.Second
	DefaultTimeoutGrace     = 5 * time.Second
	DefaultFailedAuthMax    = 10
	DefaultFailedAuthWindow = time.Minute
	DefaultGlobalRatePerSec = 10
	DefaultGlobalBurst      = 20
)

// Options configures NewRouter. Zero values select the defaults above.
type Options struct {
	Logger         *slog.Logger
	MCPServer      *mcp.Server
	Verifier       sdkauth.TokenVerifier
	AllowedOrigins []string

	FailedAuthLimiter *middleware.FailureLimiter
	GlobalLimiter     *middleware.TokenBucket
	MaxBodyBytes      int64
	RequestTimeout    time.Duration
	TimeoutGrace      time.Duration
}

// NewRouter builds the HTTP handler. Only /health and /mcp exist. A request to
// /mcp passes through, in order: Origin check, per-IP failed-auth limit,
// bearer-token authentication, the global limit, then the body limit and
// timeout, and finally the MCP handler. Authentication runs before any body
// is read.
func NewRouter(opts Options) http.Handler {
	if opts.FailedAuthLimiter == nil {
		opts.FailedAuthLimiter = middleware.NewFailureLimiter(DefaultFailedAuthMax, DefaultFailedAuthWindow)
	}
	if opts.GlobalLimiter == nil {
		opts.GlobalLimiter = middleware.NewTokenBucket(DefaultGlobalRatePerSec, DefaultGlobalBurst)
	}
	if opts.MaxBodyBytes == 0 {
		opts.MaxBodyBytes = DefaultMaxBodyBytes
	}
	if opts.RequestTimeout == 0 {
		opts.RequestTimeout = DefaultRequestTimeout
	}

	if opts.TimeoutGrace == 0 {
		opts.TimeoutGrace = DefaultTimeoutGrace
	}

	mcpHandler := mcp.NewStreamableHTTPHandler(
		func(*http.Request) *mcp.Server { return opts.MCPServer },
		&mcp.StreamableHTTPOptions{
			Stateless:           true,
			JSONResponse:        true,
			MaxRequestBodyBytes: opts.MaxBodyBytes,
		})

	authenticate := sdkauth.RequireBearerToken(opts.Verifier, nil)

	protected := chain(mcpHandler,
		middleware.Origin(opts.Logger, opts.AllowedOrigins),
		middleware.FailedAuthLimit(opts.Logger, opts.FailedAuthLimiter),
		authenticate,
		middleware.GlobalLimit(opts.GlobalLimiter),
		middleware.BodyLimit(opts.MaxBodyBytes),
		middleware.Timeout(opts.RequestTimeout, opts.TimeoutGrace),
	)

	r := chi.NewRouter()
	r.Use(chimiddleware.RequestID)
	r.Use(middleware.Logger(opts.Logger))

	r.Get("/health", Health())
	r.Handle("/mcp", protected)

	return r
}

// chain wraps h so that the first middleware listed runs first.
func chain(h http.Handler, mws ...func(http.Handler) http.Handler) http.Handler {
	for i := len(mws) - 1; i >= 0; i-- {
		h = mws[i](h)
	}

	return h
}
