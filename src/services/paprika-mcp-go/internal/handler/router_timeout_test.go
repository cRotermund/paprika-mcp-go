package handler

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/auth"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/mcpserver"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/paprikaclient/fakepaprika"
	"github.com/cRotermund/paprika-mcp-go/src/services/paprika-mcp-go/internal/testsupport"
)

const groceriesPath = "/api/v2/sync/groceries/"

func TestTimeoutsKeepTheirRelationship(t *testing.T) {
	// A write makes several Paprika calls and can queue behind hung ones, so the
	// inbound budget must hold several per-call timeouts.
	if DefaultRequestTimeout < 4*paprikaclient.DefaultRequestTimeout {
		t.Errorf(
			"inbound timeout %s is under 4x the Paprika call timeout %s",
			DefaultRequestTimeout,
			paprikaclient.DefaultRequestTimeout,
		)
	}
	if mcpserver.DefaultCallTimeout >= DefaultRequestTimeout {
		t.Errorf(
			"tool call timeout %s must end before the inbound timeout %s",
			mcpserver.DefaultCallTimeout,
			DefaultRequestTimeout,
		)
	}
	if mcpserver.DefaultCallTimeout < 3*paprikaclient.DefaultRequestTimeout {
		t.Errorf(
			"tool call timeout %s cannot hold three Paprika call timeouts",
			mcpserver.DefaultCallTimeout,
		)
	}
}

// TestHungPaprikaCallStillAnswersTheCaller sends tool calls through the real
// handler chain while Paprika hangs, and requires a well-formed tool error.
func TestHungPaprikaCallStillAnswersTheCaller(t *testing.T) {
	const hang = 10 * time.Second

	tests := []struct {
		name           string
		paprikaTimeout time.Duration
		callTimeout    time.Duration
		inboundTimeout time.Duration
	}{
		{
			name:           "Paprika call times out first",
			paprikaTimeout: 150 * time.Millisecond,
			callTimeout:    3 * time.Second,
			inboundTimeout: 5 * time.Second,
		},
		{
			name:           "tool call deadline hits first",
			paprikaTimeout: 30 * time.Second,
			callTimeout:    300 * time.Millisecond,
			inboundTimeout: 5 * time.Second,
		},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			st := testsupport.NewStackWith(
				t,
				nil,
				func(f *fakepaprika.Server) {
					f.Seed("groceryaisles", fakepaprika.SampleAisle("AISLE-1", "Produce"))
					f.Seed(
						"grocerylists",
						fakepaprika.SampleGroceryList("LIST-1", "Groceries", true),
					)
					f.Seed(
						"groceries",
						fakepaprika.SampleGrocery("G1", "carrots", "LIST-1", "AISLE-1"),
					)
				},
				func(c *paprikaclient.Config) { c.RequestTimeout = tt.paprikaTimeout },
			)

			verifier, err := auth.NewVerifier([]string{auth.HashToken(currentToken)})
			if err != nil {
				t.Fatal(err)
			}
			srv := httptest.NewServer(NewRouter(Options{
				Logger: slog.New(slog.NewTextHandler(io.Discard, nil)),
				MCPServer: mcpserver.New(
					st.Service,
					mcpserver.Options{WritesEnabled: true, CallTimeout: tt.callTimeout},
				),
				Verifier:       verifier.Verify,
				RequestTimeout: tt.inboundTimeout,
				TimeoutGrace:   2 * time.Second,
			}))
			t.Cleanup(srv.Close)

			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			session, err := mcp.NewClient(
				&mcp.Implementation{Name: "test", Version: "0"},
				nil,
			).Connect(
				ctx,
				&mcp.StreamableClientTransport{
					Endpoint:   srv.URL + "/mcp",
					HTTPClient: &http.Client{Transport: bearerTransport{token: currentToken}},
				},
				nil,
			)
			if err != nil {
				t.Fatal(err)
			}
			defer session.Close()

			st.Fake.Inject(fakepaprika.Fault{
				Method: "GET",
				Path:   groceriesPath,
				Times:  -1,
				Kind:   fakepaprika.FaultDelay,
				Delay:  hang,
			})
			st.Fake.Inject(fakepaprika.Fault{
				Method: "POST",
				Path:   groceriesPath,
				Times:  -1,
				Kind:   fakepaprika.FaultDelay,
				Delay:  hang,
			})

			start := time.Now()
			read, err := session.CallTool(ctx, &mcp.CallToolParams{Name: "list_grocery_items"})
			if err != nil {
				t.Fatalf("read tool: protocol error instead of a tool result: %v", err)
			}
			if text := resultText(read); !read.IsError || !strings.Contains(
				text,
				"temporarily unavailable",
			) {
				t.Errorf(
					"read tool = IsError %v, %q; want a temporarily unavailable error",
					read.IsError,
					text,
				)
			}

			write, err := session.CallTool(ctx, &mcp.CallToolParams{
				Name:      "add_grocery_items",
				Arguments: map[string]any{"items": []map[string]any{{"text": "2 leeks"}}},
			})
			if err != nil {
				t.Fatalf("write tool: protocol error instead of a tool result: %v", err)
			}
			if text := resultText(write); !write.IsError || !strings.Contains(
				text,
				"may or may not",
			) {
				t.Errorf(
					"write tool = IsError %v, %q; want an outcome unknown error",
					write.IsError,
					text,
				)
			}
			if n := len(st.Fake.RequestsTo("POST", groceriesPath)); n != 1 {
				t.Errorf("grocery POSTs = %d, want exactly 1", n)
			}
			if elapsed := time.Since(start); elapsed > hang/2 {
				t.Errorf("calls took %s; they should end at the first deadline", elapsed)
			}
		})
	}
}

func resultText(res *mcp.CallToolResult) string {
	var b strings.Builder
	for _, c := range res.Content {
		if tc, ok := c.(*mcp.TextContent); ok {
			b.WriteString(tc.Text)
		}
	}
	return b.String()
}
