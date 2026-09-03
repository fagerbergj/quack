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
					return fmt.Sprintf("edit_artifact: conflict - re-read and retry.\ncurrent revision: %d\ncurrent content:\n%s",
						conflict.Revision, string(conflict.Content)), nil
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

// NewWriteArtifactTool: blob writes only; structured kinds go through their
// generated write_<kind> tool (NewWriteKindTool) instead.
func NewWriteArtifactTool(c *recordstore.Client, nodeID string, coords RoundCoords) (tool.Tool, error) {
	return functiontool.New[writeArtifactArgs, string](
		functiontool.Config{
			Name:        "write_artifact",
			Description: "Write a new revision of a blob artifact (markdown, text, PDF, image - not a structured kind; use write_<kind> for those). The registry derives the id.",
		},
		func(ctx agent.Context, a writeArtifactArgs) (string, error) {
			data := []byte(a.Bytes)
			if !strings.HasPrefix(a.Mime, "text/") && a.Mime != "application/json" {
				if b, err := base64.StdEncoding.DecodeString(a.Bytes); err == nil {
					data = b
				}
			}
			lineage := recordstore.Lineage{NodeID: nodeID, Round: coords.Round, TurnID: coords.TurnID, HeadSHA: coords.HeadSHA, Author: "worker", SavedAt: time.Now().UTC()}
			id, rev, err := c.SaveBlob(ctx, a.Kind, data, a.Mime, "", lineage)
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
func NewWriteKindTool(c *recordstore.Client, nodeID, kind string, spec recordstore.KindSpec, coords RoundCoords) (tool.Tool, error) {
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
			id, rev, err := c.SaveStructured(ctx, kind, args, "", lineage)
			if err != nil {
				return "", fmt.Errorf("write_%s: %w", kind, err)
			}
			return fmt.Sprintf("ok: id=%s revision=%d", id, rev), nil
		},
	)
}

// NewWriteKindTools builds one write_<kind> tool per registered structured
// kind; a bad schema drops just that one tool rather than failing the batch.
func NewWriteKindTools(c *recordstore.Client, nodeID string, coords RoundCoords) []tool.Tool {
	var out []tool.Tool
	for _, spec := range recordstore.Kinds() {
		t, err := NewWriteKindTool(c, nodeID, spec.Name(), spec, coords)
		if err != nil {
			continue
		}
		out = append(out, t)
	}
	return out
}
