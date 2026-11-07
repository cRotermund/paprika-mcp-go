package middleware

import (
	"context"
	"log/slog"
	"net/http"
	"time"

	chimiddleware "github.com/go-chi/chi/v5/middleware"
)

type loggerKey struct{}

// Logger logs one structured line per request and stores a request-scoped
// logger in the context for handlers (see LoggerFromContext).
func Logger(logger *slog.Logger) func(next http.Handler) http.Handler {
	return func(next http.Handler) http.Handler {
		return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			start := time.Now()
			wrapped := chimiddleware.NewWrapResponseWriter(w, r.ProtoMajor)

			requestLogger := logger.With(
				"requestId",
				chimiddleware.GetReqID(r.Context()),
				"method",
				r.Method,
				"path",
				r.URL.Path,
				"route",
				r.Pattern,
			)

			ctx := context.WithValue(r.Context(), loggerKey{}, requestLogger)
			next.ServeHTTP(wrapped, r.WithContext(ctx))

			requestLogger.Info(
				"request",
				"status",
				wrapped.Status(),
				"bytes",
				wrapped.BytesWritten(),
				"duration",
				time.Since(start).String(),
			)
		})
	}
}

func LoggerFromContext(ctx context.Context) *slog.Logger {
	if logger, ok := ctx.Value(loggerKey{}).(*slog.Logger); ok {
		return logger
	}

	return slog.Default()
}
