package testsupport

import (
	"context"
	"io"
	"log/slog"
	"testing"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/mirror"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient/fakepaprika"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/service"
)

// Stack is a fake Paprika with a client, mirror, and service wired to it.
type Stack struct {
	Fake    *fakepaprika.Server
	Client  *paprikaclient.Client
	Mirror  *mirror.Mirror
	Service *service.Service
}

// NewStack builds a stack over a seeded fake and runs the first recipe pass, so
// search is ready. Logs go to logger, or are discarded when it is nil.
func NewStack(tb testing.TB, logger *slog.Logger, seed func(*fakepaprika.Server)) *Stack {
	tb.Helper()
	return NewStackWith(tb, logger, seed, nil)
}

// NewStackWith is [NewStack] with a chance to change the client configuration.
func NewStackWith(tb testing.TB, logger *slog.Logger, seed func(*fakepaprika.Server), tweak func(*paprikaclient.Config)) *Stack {
	tb.Helper()
	if logger == nil {
		logger = slog.New(slog.NewTextHandler(io.Discard, nil))
	}
	fake := fakepaprika.New(tb)
	if seed != nil {
		seed(fake)
	}
	client := ClientWith(tb, fake, logger, tweak)
	m := mirror.New(client, mirror.Options{Logger: logger})
	if err := m.Sync(context.Background()); err != nil {
		tb.Fatal(err)
	}
	return &Stack{Fake: fake, Client: client, Mirror: m, Service: service.New(m, logger, service.WithWriter(client))}
}
