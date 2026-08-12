// Package mcp exposes the orchestrator as an MCP Streamable-HTTP server (ask tool).
package mcp

import (
	"context"
	"iter"
	"net/http"

	"github.com/google/jsonschema-go/jsonschema"
	"github.com/google/uuid"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fagerbergj/quack/internal/orchestrator"
	"github.com/fagerbergj/quack/internal/stream"
)

const userID = "local"

// mrtrAnswerID: SEP-2322 MRTR input-request ID (one outstanding at a time).
const mrtrAnswerID = "answer"

// mrtrAnswerSchema: single free-text string for MRTR elicitation.
var mrtrAnswerSchema = &jsonschema.Schema{
	Type:       "object",
	Properties: map[string]*jsonschema.Schema{"answer": {Type: "string"}},
	Required:   []string{"answer"},
}

// AskInput is the `ask` tool's input.
type AskInput struct {
	Query     string `json:"query" jsonschema:"the question or task for Quack"`
	SessionID string `json:"session_id,omitempty" jsonschema:"optional conversation id to continue"`
}

// Handler builds the Streamable-HTTP MCP handler exposing the `ask` tool.
func Handler(orch *orchestrator.Orchestrator) http.Handler {
	srv := mcp.NewServer(&mcp.Implementation{Name: "quack", Version: "0.1.0"}, nil)

	mcp.AddTool(srv, &mcp.Tool{
		Name: "ask",
		Description: "Ask Quack a question; it runs the orchestrator and returns the answer. " +
			"May pause on a clarifying question (MCP multi round-trip, SEP-2322): fulfil the " +
			"elicitation and retry with the echoed request state.",
	}, askTool(orch))

	return mcp.NewStreamableHTTPHandler(func(*http.Request) *mcp.Server { return srv }, nil)
}

// askTool adapts Orchestrator to the MRTR run/pending surface.
func askTool(orch *orchestrator.Orchestrator) mcp.ToolHandlerFor[AskInput, any] {
	run := func(ctx context.Context, sessionID, message string) iter.Seq2[stream.SSEEvent, error] {
		return orch.Run(ctx, userID, sessionID, orchestrator.SourceApp, message, nil)
	}
	pending := func(ctx context.Context, sessionID string) (orchestrator.PendingQuestion, bool) {
		return orchestrator.LatestPendingQuestion(orch.PriorEvents(ctx, userID, sessionID))
	}
	return newAskHandler(run, pending)
}

// newAskHandler returns MRTR input-required on pause, testable without a live Orchestrator.
func newAskHandler(
	run func(ctx context.Context, sessionID, message string) iter.Seq2[stream.SSEEvent, error],
	pending func(ctx context.Context, sessionID string) (orchestrator.PendingQuestion, bool),
) mcp.ToolHandlerFor[AskInput, any] {
	return func(ctx context.Context, req *mcp.CallToolRequest, args AskInput) (*mcp.CallToolResult, any, error) {
		sessionID := args.SessionID
		message := args.Query

		// A retry: answer the pending question instead of re-asking.
		if resp, ok := req.Params.InputResponses[mrtrAnswerID]; ok {
			if req.Params.RequestState != "" {
				sessionID = req.Params.RequestState
			}
			elicited, _ := resp.(*mcp.ElicitResult)
			if elicited == nil || elicited.Action != "accept" {
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: "cancelled: no answer provided"}},
				}, nil, nil
			}
			answer, _ := elicited.Content["answer"].(string)
			message = answer
		}
		if sessionID == "" {
			sessionID = uuid.NewString()
		}

		var answer string
		for ev, err := range run(ctx, sessionID, message) {
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

		if pq, needsInput := pending(ctx, sessionID); needsInput {
			return &mcp.CallToolResult{
				InputRequests: mcp.InputRequestMap{
					mrtrAnswerID: &mcp.ElicitParams{
						Message:         pq.Message,
						RequestedSchema: mrtrAnswerSchema,
					},
				},
				// ponytail: RequestState carries only the session ID, already
				// client-visible via session_id - no signing needed (see
				// CallToolResult.RequestState's doc on unauthenticated servers).
				RequestState: sessionID,
			}, nil, nil
		}

		return &mcp.CallToolResult{
			Content: []mcp.Content{&mcp.TextContent{Text: answer}},
		}, nil, nil
	}
}
