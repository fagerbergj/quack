// artifacts.go: ADK-native equivalents of the ACP loopback MCP artifact
// tools (internal/acp/memorymcp.go) - same recordstore functions in thin
// agent.Context wrappers, so merge/validation/identity lives in one place (#1090 P4, #1091).
package tools

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/google/jsonschema-go/jsonschema"
	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/artifactref"
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
//
// Passed to tool constructors as a *RoundCoords, not a value: a native gated
// node's tools are built once, before its judge/revise loop starts, while the
// round/turn/head-sha/trigger-annotation are only known once the gate reaches
// that round (vetting.Config.RoundCoordsSink writes through the same pointer
// every tool closure shares) - mirrors ledger.Coords' per-round
// SetLedgerCoords restamping (#1123).
type RoundCoords struct {
	Round             int
	TurnID            string
	HeadSHA           string
	TriggerAnnotation string
}

func NewEditArtifactTool(c *recordstore.Client, nodeID string, coords *RoundCoords) (tool.Tool, error) {
	return functiontool.New[editArtifactArgs, string](
		functiontool.Config{
			Name: "edit_artifact",
			Description: "Edit an existing artifact by search/replace. Optimistic locking: if base_revision is stale, " +
				"your edits are still applied to the current latest content as long as each `old` string still matches " +
				"exactly once; a real conflict fails and returns the current content and revision to retry against. " +
				"Structured artifacts are re-validated before the write. On a structured artifact, `old`/`new` match " +
				"against each field's decoded text, not the raw serialized JSON - `new` can contain raw newlines, quotes, " +
				"or backslashes with no escaping. There is no need to rewrite the whole record with write_<kind> just to change one field.",
		},
		func(ctx agent.Context, a editArtifactArgs) (string, error) {
			if len(a.Edits) == 0 {
				return "", errors.New("edit_artifact: edits must be non-empty")
			}
			ops := make([]recordstore.EditOp, len(a.Edits))
			for i, e := range a.Edits {
				ops[i] = recordstore.EditOp{Old: e.Old, New: e.New}
			}
			lineage := recordstore.Lineage{NodeID: nodeID, Round: coords.Round, TurnID: coords.TurnID, HeadSHA: coords.HeadSHA, TriggerAnnotation: coords.TriggerAnnotation, Author: "worker", SavedAt: time.Now().UTC()}
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
	for _, spec := range recordstore.KindsForClass(recordstore.Blob) {
		kinds = append(kinds, spec.Name())
	}
	return fmt.Sprintf("Write a new revision of a blob artifact (%s - not a structured kind; use write_<kind> for those). The registry derives the id.", strings.Join(kinds, ", "))
}

// NewWriteArtifactTool: blob writes only; structured kinds go through their
// write_<kind> tool (NewWriteKindTool) instead. hint is the session-derived
// identity hint (vetting.SubjectHint(chatID)) for hint-requiring kinds (document, pr_body) - never a tool argument, like ids (#1108 finding 2).
func NewWriteArtifactTool(c *recordstore.Client, nodeID string, coords *RoundCoords, hint string) (tool.Tool, error) {
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
			spec, ok := recordstore.SpecFor(a.Kind)
			if ok && spec.System {
				return "", fmt.Errorf("write_artifact: kind %q is not writable directly", a.Kind)
			}
			lineage := recordstore.Lineage{NodeID: nodeID, Round: coords.Round, TurnID: coords.TurnID, HeadSHA: coords.HeadSHA, TriggerAnnotation: coords.TriggerAnnotation, Author: "worker", SavedAt: time.Now().UTC()}
			// Only hint-requiring blob kinds (document, pr_body) get hint -
			// hint-optional kinds (text, bytes) must keep deriving their id from
			// content, or every write collapses onto one id (#1108 finding 2, mirrors memorymcp.go).
			blobHint := ""
			if ok && spec.RequiresHint {
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
// spec's registered JSONSchema, parsed once rather than reflected from a Go
// struct (#1090 §4.4) - mirrors memorymcp.go's registerWriteKindTool.
func NewWriteKindTool(c *recordstore.Client, nodeID, kind string, spec recordstore.KindSpec, coords *RoundCoords, hint string) (tool.Tool, error) {
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
			lineage := recordstore.Lineage{NodeID: nodeID, Round: coords.Round, TurnID: coords.TurnID, HeadSHA: coords.HeadSHA, TriggerAnnotation: coords.TriggerAnnotation, Author: "worker", SavedAt: time.Now().UTC()}
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
// kind. recordstore.Register already rejects a bad JSONSchema at startup
// (#1108 finding 3), so a failure is only theoretical - surfaced, never skipped, so the two surfaces can't drift.
func NewWriteKindTools(c *recordstore.Client, nodeID string, coords *RoundCoords, hint string) ([]tool.Tool, error) {
	out := make([]tool.Tool, 0, len(recordstore.Kinds()))
	for _, spec := range recordstore.Kinds() {
		if !spec.AgentWritable {
			continue // gate-owned kinds (judge_round, delivery_record) are never worker tools
		}
		t, err := NewWriteKindTool(c, nodeID, spec.Name(), spec, coords, hint)
		if err != nil {
			return nil, fmt.Errorf("write_%s: %w", spec.Name(), err)
		}
		out = append(out, t)
	}
	return out, nil
}

// readArtifactArgs is read_artifact's input - id-addressed (from
// list_artifacts), unlike the MCP surface's filename-addressed variant,
// since recordstore is the native surface's only source of ids.
type readArtifactArgs struct {
	ID       string `json:"id"`
	Revision int    `json:"revision,omitempty"`
	// Offset/Lines window a large text artifact instead of returning it whole -
	// a window bypasses InlineMaxBytes, since only the slice is ever returned.
	Offset int `json:"offset,omitempty"`
	Lines  int `json:"lines,omitempty"`
}

// NewReadArtifactTool: the native equivalent of the MCP-only read_artifact
// tool (#1012 wired it into ACP's loopback MCP only) - same recordstore
// Client reads, same InlineMaxBytes cap as memorymcp.go's registerReadArtifactTool.
func NewReadArtifactTool(c *recordstore.Client) (tool.Tool, error) {
	return functiontool.New[readArtifactArgs, string](
		functiontool.Config{
			Name: "read_artifact",
			Description: "Read an artifact by id (from list_artifacts). Text content is returned inline; " +
				"binary content is base64-encoded. Omit revision for the latest. Pass offset (a 1-based line " +
				"number) and/or lines (window size) to read a window of a large text artifact instead of the " +
				"whole thing - a window is returned even past the inline size limit that would otherwise refuse it.",
		},
		func(ctx agent.Context, a readArtifactArgs) (string, error) {
			var data []byte
			var mime string
			var lineage recordstore.Lineage
			var ok bool
			var err error
			if a.Revision > 0 {
				data, ok, err = c.LoadVersion(ctx, a.ID, a.Revision)
			} else {
				data, mime, lineage, _, ok, err = c.LatestWithMeta(ctx, a.ID)
			}
			if err != nil {
				return "", fmt.Errorf("read_artifact: %w", err)
			}
			if !ok {
				return "", fmt.Errorf("read_artifact: %s: not found", a.ID)
			}
			// prov (a stored page's url/title/fetched_at) sits outside whatever
			// follows, never inside the counted lines an offset/grep hit indexes.
			prov := provenanceHeader(lineage, data)
			if prov != "" {
				prov += "\n\n"
			}
			return prov + shapeReadArtifact(data, mime, a), nil
		},
	)
}

// shapeReadArtifact: read_artifact's body once the artifact is found -
// windowed, too-large-refusal, or whole (base64 if binary).
func shapeReadArtifact(data []byte, mime string, a readArtifactArgs) string {
	// LoadVersion carries no stored mime; a historical revision falls back to
	// a UTF-8 sniff (ponytail: a misprint risk on a binary kind, not data loss).
	isText := (mime != "" && (strings.HasPrefix(mime, "text/") || mime == "application/json")) ||
		(mime == "" && utf8.Valid(data))
	if isText && (a.Offset > 0 || a.Lines > 0) {
		start := a.Offset
		if start < 1 {
			start = 1
		}
		return windowLines(strings.Split(string(data), "\n"), start, a.Lines, strings.Count(string(data), "\n")+1)
	}
	if len(data) > artifactref.InlineMaxBytes {
		return fmt.Sprintf("size: %d bytes (exceeds %d byte read_artifact limit)\n\nread_artifact: content too large to return inline; pass offset/lines to read a window.",
			len(data), artifactref.InlineMaxBytes)
	}
	text := string(data)
	if !isText {
		text = base64.StdEncoding.EncodeToString(data)
	}
	if mime == "" {
		return text
	}
	return fmt.Sprintf("mime: %s\n\n%s", mime, text)
}

// grepArtifactsArgs is grep_artifacts' input.
type grepArtifactsArgs struct {
	Pattern string   `json:"pattern"`
	IDs     []string `json:"ids,omitempty"`
}

// newGrepArtifacts: registry constructor for grep_artifacts.
func newGrepArtifacts(d Deps) (tool.Tool, error) {
	return NewGrepArtifactsTool(d.RecordStore)
}

// NewGrepArtifactsTool: regexes across the chat's stored web_page artifacts
// (or just ids, when given). A nil c builds fine but errors only on a call.
func NewGrepArtifactsTool(c *recordstore.Client) (tool.Tool, error) {
	return functiontool.New[grepArtifactsArgs, string](
		functiontool.Config{
			Name: "grep_artifacts",
			Description: "Search this chat's stored web_page artifacts (from a large web_fetch) for a regex " +
				"pattern (case-insensitive; falls back to a literal substring match on an invalid regex). " +
				"Searches every stored page, or only the given ids. Returns `artifact:line: text` hits; pair " +
				"with read_artifact(id, offset, lines) to read the window around a hit.",
		},
		func(ctx agent.Context, a grepArtifactsArgs) (string, error) {
			if c == nil {
				return "", errors.New("grep_artifacts: no chat artifacts service configured")
			}
			if strings.TrimSpace(a.Pattern) == "" {
				return "", errors.New("grep_artifacts: pattern must be non-empty")
			}
			ids := a.IDs
			if len(ids) == 0 {
				items, err := c.List(ctx, kindWebPage)
				if err != nil {
					return "", fmt.Errorf("grep_artifacts: %w", err)
				}
				for _, it := range items {
					ids = append(ids, it.ID)
				}
			}
			return grepArtifactIDs(ctx, c, ids, a.Pattern), nil
		},
	)
}

// grepArtifactIDs: matches pattern across every id's latest revision,
// capped at fetchGrepMaxLines total hits across all of them.
func grepArtifactIDs(ctx agent.Context, c *recordstore.Client, ids []string, pattern string) string {
	matchLine := compileGrepMatcher(pattern)
	var hits []string
	matched := 0
	capped := false
outer:
	for _, id := range ids {
		data, _, lineage, _, ok, err := c.LatestWithMeta(ctx, id)
		if err != nil || !ok {
			continue
		}
		firstForID := true
		for i, ln := range strings.Split(string(data), "\n") {
			if !matchLine(ln) {
				continue
			}
			if matched >= fetchGrepMaxLines {
				capped = true
				break outer
			}
			// One provenance line per id, before its first hit - outside the
			// per-line count so a hit's own line number stays exact.
			if firstForID {
				if prov := provenanceHeader(lineage, data); prov != "" {
					hits = append(hits, prov)
				}
				firstForID = false
			}
			hits = append(hits, fmt.Sprintf("%s:%d: %s", id, i+1, strings.TrimSpace(ln)))
			matched++
		}
	}
	if len(hits) == 0 {
		return fmt.Sprintf("[no lines match %q across %d artifact(s)]", pattern, len(ids))
	}
	footer := fmt.Sprintf("\n\n[%d matching line(s).]", matched)
	if capped {
		footer = fmt.Sprintf("\n\n[first %d matches shown (more exist) - narrow the pattern.]", fetchGrepMaxLines)
	}
	return capFetchReturn(strings.Join(hits, "\n")) + footer
}

// BuildNativeArtifactTools is the one place orchestrator and gated nodes build artifact tools (#1123).
// blobHint (DocumentHint) and structuredHint (SubjectHint) differ because each kind's save path looks up its own id.
func BuildNativeArtifactTools(c *recordstore.Client, nodeID string, coords *RoundCoords, blobHint, structuredHint string) ([]tool.Tool, error) {
	if coords == nil {
		coords = &RoundCoords{}
	}
	listTool, err := NewListArtifactsTool(c)
	if err != nil {
		return nil, fmt.Errorf("list_artifacts: %w", err)
	}
	readTool, err := NewReadArtifactTool(c)
	if err != nil {
		return nil, fmt.Errorf("read_artifact: %w", err)
	}
	editTool, err := NewEditArtifactTool(c, nodeID, coords)
	if err != nil {
		return nil, fmt.Errorf("edit_artifact: %w", err)
	}
	writeTool, err := NewWriteArtifactTool(c, nodeID, coords, blobHint)
	if err != nil {
		return nil, fmt.Errorf("write_artifact: %w", err)
	}
	kindTools, err := NewWriteKindTools(c, nodeID, coords, structuredHint)
	if err != nil {
		return nil, fmt.Errorf("write_<kind>: %w", err)
	}
	out := []tool.Tool{listTool, readTool, editTool, writeTool}
	return append(out, kindTools...), nil
}
