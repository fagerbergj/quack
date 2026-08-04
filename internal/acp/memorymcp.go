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
// per-run server from quack's orchestrator MCP mount (/api/v1/mcp) - it exists
// only to give an ACP subprocess an AGENTIC memory channel (query mid-task, stage
// a candidate the moment it's learned) alongside the existing gate-side recall
// (Store.Recall, front-loaded into the round-0 prompt) and answer-mined commit.
//
// Scoping is entirely server-side: session/new hands the round ONE url whose path
// IS an unguessable per-node secret (vetting.NewMemSecret, minted fresh by
// dag.newGatedNode - NOT the advisor-thread token: that token is derivable
// (planID+nodeID) and a worker's own prompt discloses its running siblings' node
// IDs, so it could let one node reach another's memory bucket). Every tool call
// resolves the node's Store/Scope/staging buffer from that secret via
// vetting.LookupMemSession - a registry SEPARATE from the advisor-thread one.
// Nothing about scope ever rides a tool argument.

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
// first use and kept alive for the process lifetime - one server for every
// node/round, not one per round: the URL path (the node's secret) is what
// scopes each session, so the server itself carries no per-node state. Bound
// to 127.0.0.1 only - never reachable off-host.
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

// memoryMCPHandler builds a fresh *mcp.Server per incoming session, deriving
// its tools' scope from the URL path's SECRET - resolved via
// vetting.LookupMemSession, a registry keyed by an unguessable per-node value,
// never by the advisor-thread token (see the package doc above). An unknown or
// unregistered secret (agent run outside a gated node, wrong/stale/guessed
// value, or the node already finished and drained its buffer) gets a server
// with NO tools registered - a load_memory/stage_memory call against it fails
// loudly with an explicit MCP "tool not found" error, not a silent no-op.
func memoryMCPHandler() http.Handler {
	return mcp.NewStreamableHTTPHandler(func(r *http.Request) *mcp.Server {
		secret := strings.Trim(r.URL.Path, "/")
		srv := mcp.NewServer(&mcp.Implementation{Name: "quackmcp", Version: "0.1.0"}, nil)
		sess, ok := vetting.LookupMemSession(secret)
		if !ok {
			// Never silent (#640): an unrecognized secret means either a stale/
			// wrong URL or a session that already unregistered - both worth
			// seeing, since the caller gets a tool-less server with no other signal.
			slog.Warn("acp: loopback MCP request for unknown/expired session", "component", "acp")
			return srv
		}
		// #640: the surface being OFFERED (session/new accepted it) is not proof
		// anything ever CONNECTED - that gap is exactly how a broken/renamed
		// server survived a full day of dogfooding unnoticed. Mark + log the
		// first real request against a known secret; UnregisterMemSession warns
		// if a session tears down having never reached this line.
		vetting.MarkMemSessionConnected(secret)
		slog.Info("acp: loopback MCP session connected", "component", "acp")
		// Each tool group is registered independently on the SAME per-node server,
		// gated on the session carrying its buffer: memory (#344) rides Memory, the
		// review surface (#451) rides Review. A review-only node (no memory) still
		// gets its review tools, and a memory-only node never sees the review ones.
		if sess.Memory != nil {
			mcp.AddTool(srv, &mcp.Tool{
				Name:        "load_memory",
				Description: "Recall relevant notes from shared memory about this repository/task family.",
			}, func(ctx context.Context, _ *mcp.CallToolRequest, args loadMemoryInput) (*mcp.CallToolResult, any, error) {
				text := sess.Memory.Recall(ctx, sess.Scope, args.Query)
				if text == "" {
					text = "(no relevant memory found)"
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
			})
			mcp.AddTool(srv, &mcp.Tool{
				Name:        "stage_memory",
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
			registerPRTool(srv, sess.PRStage)
		}
		return srv
	}, nil)
}

// memoryMCPServers returns the ACP mcpServers list to hand session/new - empty
// when there's no secret (the node isn't a memory participant), the agent
// didn't advertise http MCP support, or the loopback server failed to start.
// caps is the agent's negotiated capabilities from Initialize.
func memoryMCPServers(secret string, caps sdk.AgentCapabilities) []sdk.McpServer {
	// NewSessionRequest.McpServers is a required field on the wire (an omitted/
	// null array 400s) - always return a non-nil slice, even when empty.
	if secret == "" || !(caps.McpCapabilities.Http || caps.McpCapabilities.Sse) {
		return []sdk.McpServer{}
	}
	base := memoryMCPURL()
	if base == "" {
		return []sdk.McpServer{}
	}
	// opencode's session/new validates each server as an SSE-transport MCP:
	// it requires type:"sse" and a non-null headers array. The earlier Http
	// variant (type unset, headers nil) failed with -32602 Invalid params and
	// killed the subprocess, breaking every ACP node. Declare the loopback
	// memory server as SSE with explicit type + empty headers.
	return []sdk.McpServer{{Sse: &sdk.McpServerSseInline{
		Type: "sse",
		// opencode namespaces tools as "<Name>_<tool>" - surface-neutral, since
		// this one server also carries the review (#451) and PR-staging
		// surfaces, not just memory (#558). NOT "quack": opencodeEnv (serve.go)
		// names quack's own LLM provider "quack" in the SAME opencode config, so
		// a same-named MCP server collides with it in a registry we don't
		// control the internals of (#640) - "quackmcp" avoids the collision
		// outright rather than relying on opencode never minding it.
		Name:    "quackmcp",
		Url:     base + "/" + secret,
		Headers: []sdk.HttpHeader{},
	}}}
}
