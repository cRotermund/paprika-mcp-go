package mcpserver

import (
	"context"
	"testing"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

func TestPing(t *testing.T) {
	ctx := context.Background()
	clientTransport, serverTransport := mcp.NewInMemoryTransports()

	serverSession, err := New().Connect(ctx, serverTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer serverSession.Close()

	client := mcp.NewClient(&mcp.Implementation{Name: "test", Version: "0"}, nil)
	clientSession, err := client.Connect(ctx, clientTransport, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer clientSession.Close()

	res, err := clientSession.CallTool(ctx, &mcp.CallToolParams{Name: "ping"})
	if err != nil {
		t.Fatal(err)
	}
	if res.IsError {
		t.Fatalf("ping returned an error: %+v", res.Content)
	}
}
