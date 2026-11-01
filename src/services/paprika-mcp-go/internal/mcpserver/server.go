package mcpserver

import (
	"context"

	"github.com/modelcontextprotocol/go-sdk/mcp"
)

const (
	Name    = "paprika-mcp-go"
	Version = "0.1.0"
)

// New returns an MCP server with all tools registered.
func New() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{Name: Name, Version: Version}, nil)
	mcp.AddTool(s, &mcp.Tool{Name: "ping", Description: "Health check; returns pong"}, ping)

	return s
}

type pingInput struct{}

type pingOutput struct {
	Message string `json:"message"`
}

func ping(_ context.Context, _ *mcp.CallToolRequest, _ pingInput) (*mcp.CallToolResult, pingOutput, error) {
	return nil, pingOutput{Message: "pong"}, nil
}
