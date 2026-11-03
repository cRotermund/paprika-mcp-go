// Package mcpserver registers the MCP tools and maps service results and
// errors to MCP. It is the only package that knows about MCP.
package mcpserver

import (
	"context"
	"errors"
	"log/slog"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/service"
)

const (
	Name    = "paprika-mcp-go"
	Version = "0.1.0"
)

// DefaultCallTimeout bounds one tool call. The MCP SDK runs a tool under the
// session's context, not the HTTP request's, so the request deadline never
// reaches a tool; this budget does. It must end before the inbound request
// timeout (handler.DefaultRequestTimeout, 90 s) so that the tool's own result
// ("temporarily unavailable", or "outcome unknown" after a POST) is what the
// caller receives, not the middleware's generic fallback. It still exceeds the
// worst plausible write: two hung Paprika calls queued ahead (2 x 20 s) and
// the write's own hung POST (20 s).
const DefaultCallTimeout = 80 * time.Second

// Options configures New.
type Options struct {
	Logger *slog.Logger
	// CallTimeout bounds each tool call. Zero means DefaultCallTimeout.
	CallTimeout time.Duration
	// WritesEnabled registers the write tools. When false they do not exist.
	WritesEnabled bool
}

// New returns an MCP server with all tools registered. Tool errors are returned
// as tool results with IsError set, carrying the text of a service.Error.
func New(svc *service.Service, opts Options) *mcp.Server {
	if opts.Logger == nil {
		opts.Logger = slog.Default()
	}
	if opts.CallTimeout <= 0 {
		opts.CallTimeout = DefaultCallTimeout
	}
	s := mcp.NewServer(&mcp.Implementation{Name: Name, Version: Version}, nil)
	s.AddReceivingMiddleware(callTimeout(opts.CallTimeout))
	mcp.AddTool(s, &mcp.Tool{Name: "ping", Description: "Health check; returns pong", Annotations: readOnly("Ping")}, ping)

	t := &tools{svc: svc, log: opts.Logger}
	t.registerRecipes(s)
	t.registerMeals(s)
	t.registerGroceries(s)
	if opts.WritesEnabled {
		t.registerWrites(s)
	}

	return s
}

// callTimeout gives every tools/call a deadline. Services map the resulting
// context error to the same typed errors as a Paprika timeout.
func callTimeout(d time.Duration) mcp.Middleware {
	return func(next mcp.MethodHandler) mcp.MethodHandler {
		return func(ctx context.Context, method string, req mcp.Request) (mcp.Result, error) {
			if method == "tools/call" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithTimeout(ctx, d)
				defer cancel()
			}
			return next(ctx, method, req)
		}
	}
}

type tools struct {
	svc *service.Service
	log *slog.Logger
}

type pingInput struct{}

type pingOutput struct {
	Message string `json:"message"`
}

func ping(_ context.Context, _ *mcp.CallToolRequest, _ pingInput) (*mcp.CallToolResult, pingOutput, error) {
	return nil, pingOutput{Message: "pong"}, nil
}

// readOnly is the annotation set shared by every read tool.
func readOnly(title string) *mcp.ToolAnnotations {
	return &mcp.ToolAnnotations{Title: title, ReadOnlyHint: true, IdempotentHint: true}
}

// fail converts an error to the text a client sees. A service.Error carries
// client-safe text; anything else is reported generically.
func (t *tools) fail(tool string, err error) error {
	var svcErr *service.Error
	if errors.As(err, &svcErr) {
		return errors.New(svcErr.Message)
	}
	if errors.Is(err, context.Canceled) {
		return errors.New("The request was canceled.")
	}
	t.log.Error("tool failed", "tool", tool, "error", err)
	return errors.New("Internal error. The operator can find details in the server logs.")
}

// pageInput holds the paging parameters shared by the list tools.
type pageInput struct {
	Limit  int    `json:"limit,omitempty" jsonschema:"Maximum items to return (default 25, at most 100)."`
	Cursor string `json:"cursor,omitempty" jsonschema:"The next_cursor from the previous page, to get the next page."`
}

// pageOutput holds the paging fields shared by list results.
type pageOutput struct {
	Total      int    `json:"total" jsonschema:"Number of items across all pages."`
	NextCursor string `json:"next_cursor,omitempty" jsonschema:"Pass as cursor to get the next page. Absent on the last page."`
}

func pageOf[T, U any](p service.Page[T], conv func(T) U) ([]U, pageOutput) {
	items := make([]U, 0, len(p.Items))
	for _, it := range p.Items {
		items = append(items, conv(it))
	}
	return items, pageOutput{Total: p.Total, NextCursor: p.NextCursor}
}
