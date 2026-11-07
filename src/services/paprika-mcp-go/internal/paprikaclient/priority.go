package paprikaclient

import "context"

type priorityKey struct{}

// Background marks ctx so that requests made with it are background requests:
// bulk or scheduled work such as the cache's recipe passes. A background
// request takes a rate-limit token only when [BackgroundReserve] tokens would
// remain, which keeps room for interactive requests (the default). The mark
// travels with the context, so it covers every call made with it.
func Background(ctx context.Context) context.Context {
	return context.WithValue(ctx, priorityKey{}, true)
}

// IsBackground reports whether ctx was marked with [Background].
func IsBackground(ctx context.Context) bool {
	marked, _ := ctx.Value(priorityKey{}).(bool)
	return marked
}
