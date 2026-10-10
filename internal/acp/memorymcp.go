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

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/artifactschema"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/vetting"
)

// toolCheckMermaid: stateless, offered to every session regardless of
// Memory/Review/PRStage - see mcpToolNames.
const toolCheckMermaid = "check_mermaid"

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

// mcpServerName: loopback server name; the pi-acp shim prefixes tools with "<name>_".
const mcpServerName = "quackmcp"

// Tool names shared by registrations and mcpToolNames. They match the native registry names: the shim's
// "<server>_<tool>" prefix keeps them collision-free, and a worker must read the same name on either surface.
const (
	toolLoadMemory    = "load_memory"
	toolStageMemory   = "stage_memory"
	toolRecallMemory  = "recall_memory"
	toolReadArtifact  = "read_artifact"
	toolListArtifacts = "list_artifacts"
	toolEditArtifact  = "edit_artifact"
	toolWriteArtifact = "write_artifact"
	writeKindPrefix   = "write_" // + registered structured kind name, e.g. write_finding
)

// currentRound reads sess's live round/turn/head-sha off its AdvisorTask (refreshed each round);
// zero values when there is no advisor thread or it was already unregistered.
func currentRound(sess vetting.MemSession) (round int, turnID, headSHA, triggerAnnotation string) {
	if sess.AdvisorToken == "" {
		return 0, "", "", ""
	}
	t, ok := vetting.LookupAdvisorThread(sess.AdvisorToken)
	if !ok {
		return 0, "", "", ""
	}
	return t.Round, t.TurnID, t.HeadSHA, t.TriggerAnnotation
}

type readArtifactInput struct {
	Name     string `json:"name" jsonschema:"artifact filename to read"`
	Revision int64  `json:"revision,omitempty" jsonschema:"specific revision; omit for the latest"`
}

// registerReadArtifactTool exposes one node's own chat artifacts. Scope comes only from the registered
// session, never the caller, so a node can never name another chat's artifacts.
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
		// Cap before it lands in the agent's context: an artifact can be arbitrarily large (e.g. a video).
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

type listArtifactsInput struct {
	Kind string `json:"kind,omitempty" jsonschema:"only list artifacts of this registered kind; omit for all kinds"`
}

// registerListArtifactsTool lists every id in this chat, so a node can find other nodes' artifacts
// before editing one (any node may edit any output artifact).
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

type editArtifactInput struct {
	ID           string           `json:"id" jsonschema:"artifact id, from list_artifacts or read_artifact"`
	BaseRevision int              `json:"base_revision" jsonschema:"the revision you last read; used to detect a concurrent edit"`
	Edits        []editArtifactOp `json:"edits" jsonschema:"one or more search/replace pairs, applied in order"`
}

// editConflictResult is edit_artifact's success payload on a real conflict; fields mirror
// recordstore.EditConflict so the two never drift.
type editConflictResult struct {
	Conflict bool   `json:"conflict"`
	Revision int    `json:"revision"`
	Content  string `json:"content"`
}

// editArtifactOp is one search/replace pair. OldText/NewText alias old/new for workers primed on the
// MCP filesystem server's edit_file spelling, which otherwise retry the same edit under both.
type editArtifactOp struct {
	Old     string `json:"old,omitempty" jsonschema:"exact text to replace; must match exactly once in the target content"`
	New     string `json:"new,omitempty" jsonschema:"replacement text"`
	OldText string `json:"oldText,omitempty" jsonschema:"alias for old"`
	NewText string `json:"newText,omitempty" jsonschema:"alias for new"`
}

// resolve picks old/new, falling back to the oldText/newText alias. Setting both spellings is rejected:
// picking one silently risks applying an edit the caller didn't intend.
func (e editArtifactOp) resolve() (old, new string, err error) {
	if e.Old != "" && e.OldText != "" {
		return "", "", errors.New("edit_artifact: set only one of old/oldText, not both")
	}
	if e.New != "" && e.NewText != "" {
		return "", "", errors.New("edit_artifact: set only one of new/newText, not both")
	}
	old, new = e.Old, e.New
	if old == "" {
		old = e.OldText
	}
	if new == "" {
		new = e.NewText
	}
	return old, new, nil
}

// registerEditArtifactTool: optimistic-locking search/replace. A stale base_revision succeeds while every Old
// still matches uniquely against latest; a real conflict returns latest content and revision to retry from.
func registerEditArtifactTool(srv *mcp.Server, c *recordstore.Client, sess vetting.MemSession) {
	mcp.AddTool(srv, &mcp.Tool{
		Name: toolEditArtifact,
		Description: "Edit an existing artifact by search/replace. Optimistic locking: if base_revision is stale, " +
			"your edits are still applied to the current latest content as long as each `old` string still matches " +
			"exactly once; a real conflict fails and returns the current content and revision to retry against. " +
			"Structured artifacts are re-validated before the write. On a structured artifact, `old`/`new` match " +
			"against each field's decoded text, not the raw serialized JSON - `new` can contain raw newlines, quotes, " +
			"or backslashes with no escaping. There is no need to rewrite the whole record with write_<kind> just to change one field.",
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args editArtifactInput) (*mcp.CallToolResult, any, error) {
		if len(args.Edits) == 0 {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "edit_artifact: edits must be non-empty"}}}, nil, nil
		}
		ops := make([]recordstore.EditOp, len(args.Edits))
		for i, e := range args.Edits {
			old, new, err := e.resolve()
			if err != nil {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: err.Error()}}}, nil, nil
			}
			ops[i] = recordstore.EditOp{Old: old, New: new}
		}
		round, turnID, headSHA, trigger := currentRound(sess)
		lineage := recordstore.Lineage{NodeID: sess.NodeID, Round: round, TurnID: turnID, HeadSHA: headSHA, TriggerAnnotation: trigger, Author: "worker", SavedAt: time.Now().UTC()}
		rev, _, err := c.Edit(ctx, args.ID, args.BaseRevision, ops, lineage)
		if err != nil {
			var conflict *recordstore.EditConflict
			if errors.As(err, &conflict) {
				// A conflict is an actionable outcome (re-read and retry), not a tool failure: success, not IsError.
				out := editConflictResult{Conflict: true, Revision: conflict.Revision, Content: string(conflict.Content)}
				return &mcp.CallToolResult{
					Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("conflict - re-read and retry.\ncurrent revision: %d\ncurrent content:\n%s", conflict.Revision, string(conflict.Content))}},
				}, out, nil
			}
			if msg, ok := artifactschema.RefusalFromError(err); ok {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}, nil, nil
			}
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "edit_artifact: " + err.Error()}}}, nil, nil
		}
		if sess.ToolWritten != nil {
			sess.ToolWritten.Add(args.ID)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("ok: %s revision %d", args.ID, rev)}}}, nil, nil
	})
}

// write_artifact accepts blob kinds only.
type writeArtifactInput struct {
	Kind  string `json:"kind" jsonschema:"a registered blob kind - see the tool description for the current list"`
	Mime  string `json:"mime" jsonschema:"the content's mime type"`
	Bytes string `json:"bytes" jsonschema:"content: raw text for a text mime, else base64"`
}

// writeArtifactDescription lists registered Blob kinds by name so it can't drift from the registry.
func writeArtifactDescription() string {
	var kinds []string
	for _, spec := range recordstore.KindsForClass(recordstore.Blob) {
		kinds = append(kinds, spec.Name())
	}
	return fmt.Sprintf("Write a new revision of a blob artifact (%s - not a structured kind; use write_<kind> for those). The registry derives the id.", strings.Join(kinds, ", "))
}

// registerWriteArtifactTool: blob writes only; structured kinds use their generated write_<kind> tool
// so the registry validates their shape. The registry derives the id; this tool never accepts one.
func registerWriteArtifactTool(srv *mcp.Server, c *recordstore.Client, sess vetting.MemSession) {
	mcp.AddTool(srv, &mcp.Tool{
		Name:        toolWriteArtifact,
		Description: writeArtifactDescription(),
	}, func(ctx context.Context, _ *mcp.CallToolRequest, args writeArtifactInput) (*mcp.CallToolResult, any, error) {
		spec, ok := recordstore.SpecFor(args.Kind)
		if ok && spec.System {
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("write_artifact: kind %q is not writable directly", args.Kind)}}}, nil, nil
		}
		data := []byte(args.Bytes)
		if !strings.HasPrefix(args.Mime, "text/") && args.Mime != "application/json" {
			if b, err := base64.StdEncoding.DecodeString(args.Bytes); err == nil {
				data = b
			}
		}
		round, turnID, headSHA, trigger := currentRound(sess)
		lineage := recordstore.Lineage{NodeID: sess.NodeID, Round: round, TurnID: turnID, HeadSHA: headSHA, TriggerAnnotation: trigger, Author: "worker", SavedAt: time.Now().UTC()}
		// RequiresHint gets DocumentHint (matches its own save-side lookup, not
		// SubjectHint) - a hint-optional kind stays unhinted or every write collapses onto one id.
		var hint string
		if ok && spec.RequiresHint {
			hint = vetting.DocumentHint(sess.ChatID)
		}
		id, rev, err := c.SaveBlob(ctx, args.Kind, data, args.Mime, hint, lineage)
		if err != nil {
			if msg, ok := artifactschema.RefusalFromError(err); ok {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}, nil, nil
			}
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: "write_artifact: " + err.Error()}}}, nil, nil
		}
		if sess.ToolWritten != nil {
			sess.ToolWritten.Add(id)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("ok: id=%s revision=%d", id, rev)}}}, nil, nil
	})
}

// registerWriteKindTool generates write_<kind> whose input schema is the kind's registered JSONSchema (raw map
// input, so AddTool infers none). sess.ToolWritten records every id so the answer-tail fallback skips them.
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
		round, turnID, headSHA, trigger := currentRound(sess)
		lineage := recordstore.Lineage{NodeID: sess.NodeID, Round: round, TurnID: turnID, HeadSHA: headSHA, TriggerAnnotation: trigger, Author: "worker", SavedAt: time.Now().UTC()}
		// code_review's Identity requires a non-empty hint (vetting.requireHint);
		// derive it from the registered session, same as the gate - never a tool arg.
		var hint string
		if spec.RequiresHint {
			hint = vetting.SubjectHint(sess.ChatID)
		}
		// Bake the rendered markdown into this same write so the artifact panel never reimplements
		// the renderer (see RenderCodeReviewForWrite).
		if kind == "code_review" {
			args["rendered"] = vetting.RenderCodeReviewForWrite(ctx, c, args)
		}
		id, rev, err := c.SaveStructured(ctx, kind, args, hint, lineage)
		if err != nil {
			if msg, ok := artifactschema.RefusalFromError(err); ok {
				return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: msg}}}, nil, nil
			}
			return &mcp.CallToolResult{IsError: true, Content: []mcp.Content{&mcp.TextContent{Text: writeKindPrefix + kind + ": " + err.Error()}}}, nil, nil
		}
		if sess.ToolWritten != nil {
			sess.ToolWritten.Add(id)
		}
		return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: fmt.Sprintf("ok: id=%s revision=%d", id, rev)}}}, nil, nil
	})
}

// registerArtifactWriteTools wires list_artifacts, edit_artifact, write_artifact and one write_<kind>
// per structured kind onto srv, scoped to sess. Any node may edit any output artifact.
func registerArtifactWriteTools(srv *mcp.Server, sess vetting.MemSession) {
	c := recordstore.New(sess.Artifacts, sess.AppName, sess.UserID, sess.ChatID)
	if sess.Ledger != nil {
		c = c.WithLedger(sess.Ledger)
	}
	if sess.Schemas != nil {
		c = c.WithSchemas(sess.Schemas)
	}
	registerListArtifactsTool(srv, c)
	registerEditArtifactTool(srv, c, sess)
	registerWriteArtifactTool(srv, c, sess)
	for _, spec := range recordstore.Kinds() {
		if !spec.AgentWritable {
			continue // gate-only kind (judge_round, delivery_record) - never a worker tool
		}
		// A slice reviewer feeding a synthesizer never owns the delivered verdict; withholding
		// write_code_review keeps the tool list the fact its prompt tells it to trust.
		if spec.Name() == "code_review" && sess.Review != nil && sess.Review.IsNonDeliveringSlice() {
			continue
		}
		registerWriteKindTool(srv, c, sess, spec.Name(), spec)
	}
}

type loadMemoryInput struct {
	Query string `json:"query" jsonschema:"what to recall (a topic, not a document)"`
}

type stageMemoryInput struct {
	Content string `json:"content" jsonschema:"one durable, atomic fact worth remembering"`
	Kind    string `json:"kind,omitempty" jsonschema:"which bucket this belongs to: repo, role, or user (default: repo)"`
}

// recallMemoryInput is the recall_memory tool's input.
type recallMemoryInput struct {
	Query string `json:"query" jsonschema:"what to recall (a topic, not a document)"`
	K     int    `json:"k,omitempty" jsonschema:"max memories to return; capped by the server's own top_k"`
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
				text, hits := sess.Memory.RecallWithHits(ctx, sess.Scope, args.Query)
				// Recorded and voted exactly like recall_memory.
				sess.Memory.LogRecallLedgerOnly(ctx, sess.Ledger, sess.ChatID, sess.NodeID, "tool", hits)
				if sess.Recalled != nil {
					sess.Recalled.Add(hits...)
				}
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
			mcp.AddTool(srv, &mcp.Tool{
				Name:        toolRecallMemory,
				Description: "Recall up to k durable facts from shared memory relevant to `query`. Returns a compact id/tier/score/content list - cite an id in your answer when you rely on it. Every call is logged and may be voted on.",
			}, func(ctx context.Context, _ *mcp.CallToolRequest, args recallMemoryInput) (*mcp.CallToolResult, any, error) {
				hits, truncated := sess.Memory.RecallForTool(ctx, sess.Scope, args.Query, args.K)
				sess.Memory.LogRecallLedgerOnly(ctx, sess.Ledger, sess.ChatID, sess.NodeID, "tool", hits)
				if sess.Recalled != nil {
					sess.Recalled.Add(hits...)
				}
				return &mcp.CallToolResult{Content: []mcp.Content{&mcp.TextContent{Text: memory.FormatForModel(hits, truncated)}}}, nil, nil
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
