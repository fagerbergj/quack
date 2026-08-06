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

// Memory MCP surface: per-run loopback server scoped by unguessable per-node secret.

// mcpServerName: loopback server name; opencode prefixes tools with "<name>_".
const mcpServerName = "quackmcp"

// Tool names shared between registrations and mcpToolNames.
const (
	toolLoadMemory  = "load_memory"
	toolStageMemory = "stage_memory"
)

// loadMemoryInput is the load_memory tool's input.
type loadMemoryInput struct {
	Query string `json:"query" jsonschema:"what to recall (a topic, not a document)"`
}

// stageMemoryInput is the stage_memory tool's input.
type stageMemoryInput struct {
	Content string `json:"content" jsonschema:"one durable, atomic fact worth remembering"`
	Kind    string `json:"kind,omitempty" jsonschema:"which bucket this belongs to: repo, role, or user (default: repo)"`
}

// memoryMCP: process-local loopback MCP server, scoped by URL path.
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

// memoryMCPHandler builds a per-session MCP server keyed by the URL path's secret.
func memoryMCPHandler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		secret := strings.Trim(r.URL.Path, "/")
		srv := mcp.NewServer(&mcp.Implementation{Name: mcpServerName, Version: "0.1.0"}, nil)
		sess, ok := vetting.LookupMemSession(secret)
		if !ok {
			slog.Warn("acp: loopback MCP request for unknown/expired session", "component", "acp")
			return srv
		}
		vetting.MarkMemSessionConnected(secret)
		slog.Info("acp: loopback MCP session connected", "component", "acp")
		if sess.Memory != nil {
			mcp.AddTool(srv, &mcp.Tool{
				Name:        toolLoadMemory,
				Description: "Recall relevant notes from shared memory about this repository/task family.",
			}, func(ctx context.Context, _ *mcp.CallToolRequest, args loadMemoryInput) (*mcp.CallToolResult, any, error) {
				text := sess.Memory.Recall(ctx, sess.Scope, args.Query)
				if text == "" {
					text = "(no relevant memory found)"
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
			})
			mcp.AddTool(srv, &mcp.Tool{
				Name:        toolStageMemory,
				Description: "Stage a durable fact learned this run for shared memory. It is written only if this node's work is accepted.",
			}, func(ctx context.Context, _ *mcp.CallToolRequest, args stageMemoryInput) (*mcp.CallToolResult, any, error) {
				if sess.Staged == nil || strings.TrimSpace(args.Content) == "" {
					return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "memory staging unavailable for this node"}}}, nil, nil
				}
				sess.Staged.Add(memory.Candidate{Content: args.Content, Metadata: map[string]string{"bucket": args.Kind}})
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "staged"}}}, nil, nil
			})
		}
		if sess.Review != nil {
			registerReviewTools(srv, sess.Review)
		}
		if sess.PRStage != nil {
			if sess.ExistingPR {
				registerPushTool(srv, sess.PRStage)
			} else {
				registerPRTool(srv, sess.PRStage)
			}
		}
		return srv
	}, nil)
}

// memoryMCPServers returns the ACP mcpServers list, or empty when unavailable.
func memoryMCPServers(secret string, caps sdk.AgentCapabilities) []sdk.McpServer {
	if secret == "" || !(caps.McpCapabilities.Http || caps.McpCapabilities.Sse) {
		return []sdk.McpServer{}
	}
	base := memoryMCPURL()
	if base == "" {
		return []sdk.McpServer{}
	}
	return []sdk.McpServer{{Sse: &sdk.McpServerSseInline{
		Type:    "sse",
		Name:    mcpServerName,
		Url:     base + "/" + secret,
		Headers: []sdk.HttpHeader{},
	}}}
}
