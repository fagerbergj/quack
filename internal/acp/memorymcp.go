package acp

import (
	"context"
	"encoding/base64"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/vetting"
)

// toolCheckMermaid: stateless, offered to every session regardless of
// Memory/Review/PRStage - see mcpToolNames.
const toolCheckMermaid = "check_mermaid"

// checkMermaidInput is check_mermaid's input.
type checkMermaidInput struct {
	Diagram string `json:"diagram" jsonschema:"one mermaid diagram's source, without the surrounding fence"`
}

// registerCheckMermaidTool validates a diagram against the same parser the
// delivery gate runs (vetting.CheckMermaid) - pass-tool == pass-gate.
func registerCheckMermaidTool(srv *mcp.Server) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolCheckMermaid,
		Description: "Validate one mermaid diagram's source before including it in your final answer. Returns \"ok\" or a parse error with line/column. Call this on every mermaid diagram before submitting.",
	}, func(_ context.Context, _ *mcp.CallToolRequest, args checkMermaidInput) (*mcp.CallToolResult, any, error) {
		ok, line, col, msg := vetting.CheckMermaid(args.Diagram)
		if ok {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "ok"}}}, nil, nil
		}
		text := "invalid: " + msg
		if line > 0 {
			text = fmt.Sprintf("invalid (line %d, column %d): %s", line, col, msg)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: text}}}, nil, nil
	})
}

// Memory MCP surface: per-run loopback server scoped by unguessable per-node secret.

// mcpServerName: loopback server name; opencode prefixes tools with "<name>_".
const mcpServerName = "quackmcp"

// Tool names shared between registrations and mcpToolNames.
const (
	toolLoadMemory   = "load_memory"
	toolStageMemory  = "stage_memory"
	toolReadArtifact = "read_artifact"
)

// readArtifactInput is the read_artifact tool's input.
type readArtifactInput struct {
	Name     string `json:"name" jsonschema:"artifact filename to read"`
	Revision int64  `json:"revision,omitempty" jsonschema:"specific revision; omit for the latest"`
}

// registerReadArtifactTool exposes one node's own chat artifacts. Scope
// (app/user/chat) comes only from the registered session, never the caller -
// a node can never name another chat's artifacts.
func registerReadArtifactTool(srv *mcp.Server, svc artifact.Service, appName, userID, chatID string) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolReadArtifact,
		Description: "Read an artifact previously saved to this chat by name. Text content is returned inline; binary content is base64-encoded.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args readArtifactInput) (*mcp.CallToolResult, any, error) {
		resp, err := svc.Load(ctx, &artifact.LoadRequest{AppName: appName, UserID: userID, SessionID: chatID, FileName: args.Name, Version: args.Revision})
		if err != nil {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "read_artifact: " + err.Error()}}}, nil, nil
		}
		if resp.Part == nil || resp.Part.InlineData == nil {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "read_artifact: artifact has no inline content"}}}, nil, nil
		}
		mime := resp.Part.InlineData.MIMEType
		data := resp.Part.InlineData.Data
		// Cap before it lands in the agent's context - an artifact can be arbitrarily
		// large (e.g. a video), and the repo already paid for one unbounded-output incident.
		if len(data) > artifactref.InlineMaxBytes {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(
				"mime: %s\nsize: %d bytes (exceeds %d byte read_artifact limit)\n\nread_artifact: content too large to return inline; work with it on disk instead.",
				mime, len(data), artifactref.InlineMaxBytes)}}}, nil, nil
		}
		text := string(data)
		if !strings.HasPrefix(mime, "text/") && mime != "application/json" {
			text = base64.StdEncoding.EncodeToString(data)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("mime: %s\n\n%s", mime, text)}}}, nil, nil
	})
}

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
		registerCheckMermaidTool(srv) // stateless, offered even to an unknown/expired session
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
		if sess.Artifacts != nil {
			registerReadArtifactTool(srv, sess.Artifacts, sess.AppName, sess.UserID, sess.ChatID)
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
