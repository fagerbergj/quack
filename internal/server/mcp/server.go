// Package mcp exposes Quack's orchestrator as an MCP server over the Streamable
// HTTP transport. M0 provides a single `ask` tool that runs the orchestrator and
// returns the accumulated answer (it reuses the same orchestrator + event
// translation as the REST surface).
package mcp

import (
	"context"
	"net/http"

	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/stream"
)

const userID = "local"

// AskInput is the `ask` tool's input.
type AskInput struct {
	Query     string `json:"query" jsonschema:"the question or task for Quack"`
	SessionID string `json:"session_id,omitempty" jsonschema:"optional conversation id to continue"`
}

// Handler builds the Streamable-HTTP MCP handler exposing the `ask` tool.
func Handler(orch *orchestrator.Orchestrator) http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "quack", Version: "0.1.0"}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name:        "ask",
		Description: "Ask Quack a question; it runs the orchestrator and returns the answer.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args AskInput) (*mcp.CallToolResult, any, error) {
		sessionID := args.SessionID
		if sessionID == "" {
			sessionID = uuid.NewString()
		}
		var answer string
		for ev, err := range orch.Run(ctx, userID, sessionID, args.Query) {
			if err != nil {
				return &mcp.CallToolResult{
					IsError: true,
					Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}},
				}, nil, nil
			}
			if td, ok := ev.Data.(stream.AgentTokenData); ok {
				answer += td.Text
			}
		}
		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: answer}},
		}, nil, nil
	})

	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
}
