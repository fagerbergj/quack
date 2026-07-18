package acp

import (
	"context"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"

	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/vetting"
)

// Package doc (see acp.go): the memory MCP surface (#344) is a SEPARATE, narrow
// per-run server from quack's orchestrator MCP mount (/api/v1/mcp) — it exists
// only to give an ACP subprocess an AGENTIC memory channel (query mid-task, stage
// a candidate the moment it's learned) alongside the existing gate-side recall
// (Store.Recall, front-loaded into the round-0 prompt) and answer-mined commit.
//
// Scoping is entirely server-side: session/new hands the round ONE url whose path
// IS the node's advisor-thread token (the same token/registry an ACP round already
// uses to resolve its cwd — see resolveCwd), and every tool call resolves the
// node's Store/Scope/staging buffer from THAT token. Nothing about scope ever
// rides a tool argument.

// loadMemoryInput is the load_memory tool's input.
type loadMemoryInput struct {
	Query string `json:"query" jsonschema:"what to recall (a topic, not a document)"`
}

// stageMemoryInput is the stage_memory tool's input.
type stageMemoryInput struct {
	Content string `json:"content" jsonschema:"one durable, atomic fact worth remembering"`
	Kind    string `json:"kind,omitempty" jsonschema:"which bucket this belongs to: repo, role, or user (default: repo)"`
}

// memoryMCP is the process-local loopback HTTP MCP server, started lazily on
// first use and kept alive for the process lifetime — one server for every
// node/round, not one per round: the URL path (the advisor-thread token) is
// what scopes each session, so the server itself carries no per-node state.
var memoryMCP struct {
	once sync.Once
	url  string // "" if the server never started (best-effort: a round just runs without it)
}

// memoryMCPURL lazily starts the server and returns its base URL ("" on failure).
func memoryMCPURL() string {
	memoryMCP.once.Do(func() {
		ln, err := net.Listen("tcp", "127.0.0.1:0")
		if err != nil {
			slog.Warn("acp: memory MCP server failed to start; sessions run without load_memory/stage_memory", "component", "acp", "err", err)
			return
		}
		srv := &http.Server{Handler: memoryMCPHandler()}
		go func() { _ = srv.Serve(ln) }()
		memoryMCP.url = "http://" + ln.Addr().String()
	})
	return memoryMCP.url
}

// memoryMCPHandler builds a fresh *mcp.Server per incoming session, deriving its
// tools' scope from the URL path token — resolved via the SAME registry
// vetting.RegisterAdvisorThread populates for ask_advisor/the guard ladder. An
// unknown or unregistered token (agent run outside a gated node, or the node
// already finished) gets a server with no tools rather than an error, so a
// stray call degrades gracefully instead of wedging the round.
func memoryMCPHandler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		token := strings.Trim(r.URL.Path, "/")
		srv := mcp.NewServer(&mcp.Implementation{Name: "quack-memory", Version: "0.1.0"}, nil)
		task, ok := vetting.LookupAdvisorThread(token)
		if !ok || task.Memory == nil {
			return srv
		}
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "load_memory",
			Description: "Recall relevant notes from shared memory about this repository/task family.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, args loadMemoryInput) (*mcp.CallToolResult, any, error) {
			text := task.Memory.Recall(ctx, task.MemoryScope, args.Query)
			if text == "" {
				text = "(no relevant memory found)"
			}
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
		})
		mcp.AddTool(srv, &mcp.Tool{
			Name:        "stage_memory",
			Description: "Stage a durable fact learned this run for shared memory. It is written only if this node's work is accepted.",
		}, func(ctx context.Context, _ *mcp.CallToolRequest, args stageMemoryInput) (*mcp.CallToolResult, any, error) {
			if task.Staged == nil || strings.TrimSpace(args.Content) == "" {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "memory staging unavailable for this node"}}}, nil, nil
			}
			task.Staged.Add(memory.Candidate{Content: args.Content, Metadata: map[string]string{"bucket": args.Kind}})
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "staged"}}}, nil, nil
		})
		return srv
	}, nil)
}

// memoryMCPServers returns the ACP mcpServers list to hand session/new — empty
// when there's no advisor-thread token (the agent ran outside a gated node), the
// agent didn't advertise http MCP support, or the loopback server failed to
// start. caps is the agent's negotiated capabilities from Initialize.
func memoryMCPServers(token string, caps sdk.AgentCapabilities) []sdk.McpServer {
	// NewSessionRequest.McpServers is a required field on the wire (an omitted/
	// null array 400s) — always return a non-nil slice, even when empty.
	if token == "" || !caps.McpCapabilities.Http {
		return []sdk.McpServer{}
	}
	base := memoryMCPURL()
	if base == "" {
		return []sdk.McpServer{}
	}
	return []sdk.McpServer{{Http: &sdk.McpServerHttpInline{
		Name: "quack-memory",
		Url:  base + "/" + token,
	}}}
}
