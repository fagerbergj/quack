// artifacts.go: ADK-native equivalents of the ACP loopback MCP artifact
// tools (internal/acp/memorymcp.go) - same recordstore functions, thin
// agent.Context wrappers, so the merge/validation/identity logic lives in
// exactly one place (#1090 P4, issue #1091).
package tools

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/recordstore"
)

// listArtifactsArgs is list_artifacts' input.
type listArtifactsArgs struct {
	Kind string `json:"kind,omitempty"`
}

// NewListArtifactsTool: same recordstore.Client.List the MCP tool calls.
func NewListArtifactsTool(c *recordstore.Client) (tool.Tool, error) {
	return functiontool.New[listArtifactsArgs, string](
		functiontool.Config{
			Name:        "list_artifacts",
			Description: "List this chat's artifacts (id, kind, latest revision, authoring node), optionally filtered by kind.",
		},
		func(ctx agent.Context, a listArtifactsArgs) (string, error) {
			items, err := c.List(ctx, a.Kind)
			if err != nil {
				return "", fmt.Errorf("list_artifacts: %w", err)
			}
			if len(items) == 0 {
				return "(no artifacts)", nil
			}
			var b strings.Builder
			for _, it := range items {
				fmt.Fprintf(&b, "%s\trevision=%d\tkind=%s\tnode=%s\n", it.ID, it.Revision, it.Kind, it.NodeID)
			}
			return b.String(), nil
		},
	)
}

// editArtifactArgs is edit_artifact's input.
type editArtifactArgs struct {
	ID           string             `json:"id"`
	BaseRevision int                `json:"base_revision"`
	Edits        []editArtifactEdit `json:"edits"`
}

type editArtifactEdit struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// NewEditArtifactTool: same optimistic-locking merge as the MCP edit_artifact
// tool (recordstore.Client.Edit) - see internal/acp/memorymcp.go for the
// merge algorithm's description.
// RoundCoords is the gate's per-round lineage stamp (round/turn/head-sha) -
// vetting.AdvisorTask's own coordinates for a node running inside a
// judge/revise round; the zero value ({}) is correct for a caller with no
// round concept (e.g. the top-level orchestrator agent) rather than a
// hardcoded literal (#1091 adversarial review finding #4).
type RoundCoords struct {
	Round   int
	TurnID  string
	HeadSHA string
}

func NewEditArtifactTool(c *recordstore.Client, nodeID string, coords RoundCoords) (tool.Tool, error) {
	return functiontool.New[editArtifactArgs, string](
		functiontool.Config{
			Name: "edit_artifact",
			Description: "Edit an existing artifact by search/replace. Optimistic locking: if base_revision is stale, " +
				"your edits are still applied to the current latest content as long as each `old` string still matches " +
				"exactly once; a real conflict fails and returns the current content and revision to retry against.",
		},
		func(ctx agent.Context, a editArtifactArgs) (string, error) {
			if len(a.Edits) == 0 {
				return "", errors.New("edit_artifact: edits must be non-empty")
			}
			ops := make([]recordstore.EditOp, len(a.Edits))
			for i, e := range a.Edits {
				ops[i] = recordstore.EditOp{Old: e.Old, New: e.New}
			}
			lineage := recordstore.Lineage{NodeID: nodeID, Round: coords.Round, TurnID: coords.TurnID, HeadSHA: coords.HeadSHA, Author: "worker", SavedAt: time.Now().UTC()}
			rev, _, err := c.Edit(ctx, a.ID, a.BaseRevision, ops, lineage)
			if err != nil {
				var conflict *recordstore.EditConflict
				if errors.As(err, &conflict) {
					// A conflict is an expected, actionable outcome (re-read and retry with
					// fresh edits), not a tool failure - success, not an error (#1108 finding 3,
					// matches the MCP surface's editConflictResult field names).
					out := struct {
						Conflict bool   `json:"conflict"`
						Revision int    `json:"revision"`
						Content  string `json:"content"`
					}{Conflict: true, Revision: conflict.Revision, Content: string(conflict.Content)}
					b, mErr := json.Marshal(out)
					if mErr != nil {
						return "", fmt.Errorf("edit_artifact: marshaling conflict: %w", mErr)
					}
					return string(b), nil
				}
				return "", fmt.Errorf("edit_artifact: %w", err)
			}
			return fmt.Sprintf("ok: %s revision %d", a.ID, rev), nil
		},
	)
}

// writeArtifactArgs is write_artifact's input - blob kinds only.
type writeArtifactArgs struct {
	Kind  string `json:"kind"`
	Mime  string `json:"mime"`
	Bytes string `json:"bytes"`
}

// writeArtifactDescription lists the registered Blob kinds by name instead of
// a hand-written example list, so it can't drift from what the registry
// actually holds (#1108 finding 2, mirrors internal/acp/memorymcp.go).
func writeArtifactDescription() string {
	var kinds []string
	for _, spec := range recordstore.Kinds() {
		if spec.Class == recordstore.Blob {
			kinds = append(kinds, spec.Name())
		}
	}
	return fmt.Sprintf("Write a new revision of a blob artifact (%s - not a structured kind; use write_<kind> for those). The registry derives the id.", strings.Join(kinds, ", "))
}

// NewWriteArtifactTool: blob writes only; structured kinds go through their
// generated write_<kind> tool (NewWriteKindTool) instead. hint is the
// session-derived identity hint (vetting.SubjectHint(chatID)) for kinds
// whose Identity func requires one (e.g. document, pr_body) - never a tool
// argument, same principle as ids (#1108 finding 2).
func NewWriteArtifactTool(c *recordstore.Client, nodeID string, coords RoundCoords, hint string) (tool.Tool, error) {
	return functiontool.New[writeArtifactArgs, string](
		functiontool.Config{
			Name:        "write_artifact",
			Description: writeArtifactDescription(),
		},
		func(ctx agent.Context, a writeArtifactArgs) (string, error) {
			data := []byte(a.Bytes)
			if !strings.HasPrefix(a.Mime, "text/") && a.Mime != "application/json" {
				if b, err := base64.StdEncoding.DecodeString(a.Bytes); err == nil {
					data = b
				}
			}
			lineage := recordstore.Lineage{NodeID: nodeID, Round: coords.Round, TurnID: coords.TurnID, HeadSHA: coords.HeadSHA, Author: "worker", SavedAt: time.Now().UTC()}
			// Only a hint-requiring blob kind (document, pr_body) gets hint - a
			// hint-optional kind (text, bytes) must keep deriving its id from
			// content, or every write from this session collapses onto one id
			// (#1108 finding 2, mirrors internal/acp/memorymcp.go).
			blobHint := ""
			if spec, ok := recordstore.SpecFor(a.Kind); ok && spec.RequiresHint {
				blobHint = hint
			}
			id, rev, err := c.SaveBlob(ctx, a.Kind, data, a.Mime, blobHint, lineage)
			if err != nil {
				return "", fmt.Errorf("write_artifact: %w", err)
			}
			return fmt.Sprintf("ok: id=%s revision=%d", id, rev), nil
		},
	)
}

// NewWriteKindTool generates one write_<kind> tool whose input schema IS
// spec's registered JSONSchema, parsed once here rather than reflected from
// a Go struct (#1090 §4.4) - mirrors internal/acp/memorymcp.go's
// registerWriteKindTool.
func NewWriteKindTool(c *recordstore.Client, nodeID, kind string, spec recordstore.KindSpec, coords RoundCoords, hint string) (tool.Tool, error) {
	var schema jsonschema.Schema
	if err := json.Unmarshal([]byte(spec.JSONSchema), &schema); err != nil {
		return nil, fmt.Errorf("write_%s: bad JSONSchema: %w", kind, err)
	}
	return functiontool.New[map[string]any, string](
		functiontool.Config{
			Name:        "write_" + kind,
			Description: fmt.Sprintf("Write a new revision of a %q artifact. The registry validates the body and derives the id.", kind),
			InputSchema: &schema,
		},
		func(ctx agent.Context, args map[string]any) (string, error) {
			lineage := recordstore.Lineage{NodeID: nodeID, Round: coords.Round, TurnID: coords.TurnID, HeadSHA: coords.HeadSHA, Author: "worker", SavedAt: time.Now().UTC()}
			structuredHint := ""
			if spec.RequiresHint {
				structuredHint = hint
			}
			id, rev, err := c.SaveStructured(ctx, kind, args, structuredHint, lineage)
			if err != nil {
				return "", fmt.Errorf("write_%s: %w", kind, err)
			}
			return fmt.Sprintf("ok: id=%s revision=%d", id, rev), nil
		},
	)
}

// NewWriteKindTools builds one write_<kind> tool per registered structured
// kind. recordstore.Register already rejects a bad JSONSchema at process
// startup (#1108 finding 3), so NewWriteKindTool can't fail here in
// practice; a failure is still surfaced (never silently dropped) rather than
// skipped, so the two surfaces can never drift again.
func NewWriteKindTools(c *recordstore.Client, nodeID string, coords RoundCoords, hint string) ([]tool.Tool, error) {
	out := make([]tool.Tool, 0, len(recordstore.Kinds()))
	for _, spec := range recordstore.Kinds() {
		t, err := NewWriteKindTool(c, nodeID, spec.Name(), spec, coords, hint)
		if err != nil {
			return nil, fmt.Errorf("write_%s: %w", spec.Name(), err)
		}
		out = append(out, t)
	}
	return out, nil
}
