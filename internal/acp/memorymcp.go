package acp

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"strings"
	"sync"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	"github.com/google/jsonschema-go/jsonschema"
	"github.com/modelcontextprotocol/go-sdk/mcp"
	"google.golang.org/adk/v2/artifact"

	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/recordstore"
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
	toolLoadMemory    = "load_memory"
	toolStageMemory   = "stage_memory"
	toolReadArtifact  = "read_artifact"
	toolListArtifacts = "list_artifacts"
	toolEditArtifact  = "edit_artifact"
	toolWriteArtifact = "write_artifact"
	writeKindPrefix   = "write_" // + registered structured kind name, e.g. write_finding
)

// currentRound reads sess's live round/turn/head-sha off its AdvisorTask
// (SetAdvisorThreadRound, refreshed by the gate at the start of every round) -
// zero values if there's no advisor thread (sess.AdvisorToken == "") or it
// has already been unregistered (#1091 adversarial review finding #4).
func currentRound(sess vetting.MemSession) (round int, turnID, headSHA string) {
	if sess.AdvisorToken == "" {
		return 0, "", ""
	}
	t, ok := vetting.LookupAdvisorThread(sess.AdvisorToken)
	if !ok {
		return 0, "", ""
	}
	return t.Round, t.TurnID, t.HeadSHA
}

// readArtifactMaxBytes bounds the raw artifact bytes returned inline, so a
// large artifact (e.g. video) can't flood the agent's context.
const readArtifactMaxBytes = 256 * 1024

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
		if len(data) > readArtifactMaxBytes {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(
				"mime: %s\nsize: %d bytes (exceeds %d byte read_artifact limit)\n\nread_artifact: content too large to return inline; work with it on disk instead.",
				mime, len(data), readArtifactMaxBytes)}}}, nil, nil
		}
		text := string(data)
		if !strings.HasPrefix(mime, "text/") && mime != "application/json" {
			text = base64.StdEncoding.EncodeToString(data)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("mime: %s\n\n%s", mime, text)}}}, nil, nil
	})
}

// listArtifactsInput is the list_artifacts tool's input.
type listArtifactsInput struct {
	Kind string `json:"kind,omitempty" jsonschema:"only list artifacts of this registered kind; omit for all kinds"`
}

// registerListArtifactsTool exposes every id in this chat, letting a node
// discover artifacts written by other nodes before editing one (#1090 §4.4:
// any node may edit any output artifact).
func registerListArtifactsTool(srv *mcp.Server, c *recordstore.Client) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolListArtifacts,
		Description: "List this chat's artifacts (id, kind, latest revision, authoring node), optionally filtered by kind.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args listArtifactsInput) (*mcp.CallToolResult, any, error) {
		items, err := c.List(ctx, args.Kind)
		if err != nil {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "list_artifacts: " + err.Error()}}}, nil, nil
		}
		if len(items) == 0 {
			return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: "(no artifacts)"}}}, nil, nil
		}
		var b strings.Builder
		for _, it := range items {
			fmt.Fprintf(&b, "%s\trevision=%d\tkind=%s\tnode=%s\n", it.ID, it.Revision, it.Kind, it.NodeID)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: b.String()}}}, nil, nil
	})
}

// editArtifactInput is the edit_artifact tool's input.
type editArtifactInput struct {
	ID           string           `json:"id" jsonschema:"artifact id, from list_artifacts or read_artifact"`
	BaseRevision int              `json:"base_revision" jsonschema:"the revision you last read; used to detect a concurrent edit"`
	Edits        []editArtifactOp `json:"edits" jsonschema:"one or more search/replace pairs, applied in order"`
}

// editArtifactOp is one search/replace pair.
type editArtifactOp struct {
	Old string `json:"old" jsonschema:"exact text to replace; must match exactly once in the target content"`
	New string `json:"new" jsonschema:"replacement text"`
}

// registerEditArtifactTool: optimistic-locking search/replace (#1090 §4.4/§9).
// A stale base_revision still succeeds as long as every Old snippet still
// matches uniquely against the CURRENT latest revision - only a real
// conflict (ambiguous or vanished match) fails, returning the latest content
// and revision so the caller can re-read and retry.
func registerEditArtifactTool(srv *mcp.Server, c *recordstore.Client, sess vetting.MemSession) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: toolEditArtifact,
		Description: "Edit an existing artifact by search/replace. Optimistic locking: if base_revision is stale, " +
			"your edits are still applied to the current latest content as long as each `old` string still matches " +
			"exactly once; a real conflict fails and returns the current content and revision to retry against. " +
			"Structured artifacts are re-validated before the write.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args editArtifactInput) (*mcp.CallToolResult, any, error) {
		if len(args.Edits) == 0 {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "edit_artifact: edits must be non-empty"}}}, nil, nil
		}
		ops := make([]recordstore.EditOp, len(args.Edits))
		for i, e := range args.Edits {
			ops[i] = recordstore.EditOp{Old: e.Old, New: e.New}
		}
		round, turnID, headSHA := currentRound(sess)
		lineage := recordstore.Lineage{NodeID: sess.NodeID, Round: round, TurnID: turnID, HeadSHA: headSHA, Author: "worker", SavedAt: time.Now().UTC()}
		rev, _, err := c.Edit(ctx, args.ID, args.BaseRevision, ops, lineage)
		if err != nil {
			var conflict *recordstore.EditConflict
			if errors.As(err, &conflict) {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf(
					"edit_artifact: conflict - re-read and retry.\ncurrent revision: %d\ncurrent content:\n%s",
					conflict.Revision, string(conflict.Content))}}}, nil, nil
			}
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "edit_artifact: " + err.Error()}}}, nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("ok: %s revision %d", args.ID, rev)}}}, nil, nil
	})
}

// writeArtifactInput is the write_artifact tool's input - blob kinds only.
type writeArtifactInput struct {
	Kind  string `json:"kind" jsonschema:"a registered blob kind - see the tool description for the current list"`
	Mime  string `json:"mime" jsonschema:"the content's mime type"`
	Bytes string `json:"bytes" jsonschema:"content: raw text for a text mime, else base64"`
}

// writeArtifactDescription lists the registered Blob kinds by name instead of
// a hand-written example list, so it can't drift from what the registry
// actually holds (#1108 finding 2).
func writeArtifactDescription() string {
	var kinds []string
	for _, spec := range recordstore.Kinds() {
		if spec.Class == recordstore.Blob {
			kinds = append(kinds, spec.Name())
		}
	}
	return fmt.Sprintf("Write a new revision of a blob artifact (%s - not a structured kind; use write_<kind> for those). The registry derives the id.", strings.Join(kinds, ", "))
}

// registerWriteArtifactTool: blob writes only - structured kinds go through
// their generated write_<kind> tool instead, so the registry validates
// their shape. The registry derives the id; this tool never accepts one.
func registerWriteArtifactTool(srv *mcp.Server, c *recordstore.Client, sess vetting.MemSession) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolWriteArtifact,
		Description: writeArtifactDescription(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args writeArtifactInput) (*mcp.CallToolResult, any, error) {
		data := []byte(args.Bytes)
		if !strings.HasPrefix(args.Mime, "text/") && args.Mime != "application/json" {
			if b, err := base64.StdEncoding.DecodeString(args.Bytes); err == nil {
				data = b
			}
		}
		round, turnID, headSHA := currentRound(sess)
		lineage := recordstore.Lineage{NodeID: sess.NodeID, Round: round, TurnID: turnID, HeadSHA: headSHA, Author: "worker", SavedAt: time.Now().UTC()}
		// Only a hint-requiring blob kind (document, pr_body) gets the session's
		// subject hint - a hint-optional kind (text, bytes) must keep deriving its
		// id from content, or every write from this chat would collapse onto one
		// id (#1108 finding 2).
		var hint string
		if spec, ok := recordstore.SpecFor(args.Kind); ok && spec.RequiresHint {
			hint = vetting.SubjectHint(sess.ChatID)
		}
		id, rev, err := c.SaveBlob(ctx, args.Kind, data, args.Mime, hint, lineage)
		if err != nil {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "write_artifact: " + err.Error()}}}, nil, nil
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("ok: id=%s revision=%d", id, rev)}}}, nil, nil
	})
}

// registerWriteKindTool generates one write_<kind> tool whose input schema
// IS the kind's registered JSONSchema (#1090 §4.4) - parsed once at
// registration, not reflected from a Go struct, so the agent sees exactly
// the schema the record type owns. Input is a raw JSON object (map), so
// AddTool doesn't infer a struct schema over it and the parsed schema wins.
// registerWriteKindTool generates the write_<kind> tool. sess.ToolFindings
// (when non-nil) records every id written here so saveCodeReviewRound's
// answer-tail fallback can tell a tool-written id apart from a tail-only one
// (#1091 adversarial review finding #1) - tracked for every kind, not just
// "finding", since the fallback decision only needs to ask "is this id
// already accounted for."
func registerWriteKindTool(srv *mcp.Server, c *recordstore.Client, sess vetting.MemSession, kind string, spec recordstore.KindSpec) {
	var schema jsonschema.Schema
	if err := json.Unmarshal([]byte(spec.JSONSchema), &schema); err != nil {
		slog.Warn("acp: write_<kind> tool skipped - bad JSONSchema", "component", "acp", "kind", kind, "err", err)
		return
	}
	mcp.AddTool(srv, &mcp.Tool{
		Name:        writeKindPrefix + kind,
		Description: fmt.Sprintf("Write a new revision of a %q artifact. The registry validates the body and derives the id.", kind),
		InputSchema: &schema,
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args map[string]any) (*mcp.CallToolResult, any, error) {
		round, turnID, headSHA := currentRound(sess)
		lineage := recordstore.Lineage{NodeID: sess.NodeID, Round: round, TurnID: turnID, HeadSHA: headSHA, Author: "worker", SavedAt: time.Now().UTC()}
		// code_review's Identity requires a non-empty hint (vetting.requireHint);
		// derive it from the registered session, same as the gate - never a tool arg.
		var hint string
		if spec.RequiresHint {
			hint = vetting.SubjectHint(sess.ChatID)
		}
		id, rev, err := c.SaveStructured(ctx, kind, args, hint, lineage)
		if err != nil {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: writeKindPrefix + kind + ": " + err.Error()}}}, nil, nil
		}
		if sess.ToolFindings != nil {
			sess.ToolFindings.Add(id)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("ok: id=%s revision=%d", id, rev)}}}, nil, nil
	})
}

// registerArtifactWriteTools wires list_artifacts, edit_artifact,
// write_artifact and one write_<kind> per registered structured kind onto
// srv, scoped to sess's session (#1090 §4.4). Any node may edit any output
// artifact - no per-node ownership check (V4 §4.4).
func registerArtifactWriteTools(srv *mcp.Server, sess vetting.MemSession) {
	c := recordstore.New(sess.Artifacts, sess.AppName, sess.UserID, sess.ChatID)
	registerListArtifactsTool(srv, c)
	registerEditArtifactTool(srv, c, sess)
	registerWriteArtifactTool(srv, c, sess)
	for _, spec := range recordstore.Kinds() {
		registerWriteKindTool(srv, c, sess, spec.Name(), spec)
	}
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
			registerArtifactWriteTools(srv, sess)
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
