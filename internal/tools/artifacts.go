// Native twins of the ACP loopback MCP artifact tools (internal/acp/memorymcp.go): thin wrappers over the
// same recordstore calls, so merge/validation/identity live in one place.
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
	"github.com/fagerbergj/quack/internal/artifactschema"
	"github.com/fagerbergj/quack/internal/recordstore"
)

type listArtifactsArgs struct {
	Kind string `json:"kind,omitempty"`
}

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

type editArtifactArgs struct {
	ID           string             `json:"id"`
	BaseRevision int                `json:"base_revision"`
	Edits        []editArtifactEdit `json:"edits"`
}

type editArtifactEdit struct {
	Old string `json:"old"`
	New string `json:"new"`
}

// RoundCoords: the gate's per-round lineage stamp; zero outside a judge/revise round. Passed as a pointer:
// tools are built before the loop, and RoundCoordsSink writes each round through it.
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
				"or backslashes with no escaping. There is no need to rewrite the whole record with write_<kind> just to change one field. " +
				"This is how you append a long deliverable's next section after write_artifact writes the skeleton.",
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
					// A conflict is actionable (re-read and retry), so it's a result, not an error; fields match the MCP's.
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
				if msg, ok := artifactschema.RefusalFromError(err); ok {
					return "", errors.New(msg)
				}
				return "", fmt.Errorf("edit_artifact: %w", err)
			}
			return fmt.Sprintf("ok: %s revision %d", a.ID, rev), nil
		},
	)
}

type writeArtifactArgs struct {
	Kind  string `json:"kind"`
	Mime  string `json:"mime"`
	Bytes string `json:"bytes"`
}

// writeArtifactDescription names the registered Blob kinds, so it can't drift from the registry.
func writeArtifactDescription() string {
	var kinds []string
	for _, spec := range recordstore.KindsForClass(recordstore.Blob) {
		kinds = append(kinds, spec.Name())
	}
	return fmt.Sprintf("Write a new revision of a blob artifact (%s - not a structured kind; use write_<kind> for those). The registry derives the id. "+
		"A long deliverable is written in sections: write_artifact the skeleton, then edit_artifact to append each section. "+
		"Your final reply is the deliverable, or - when it lives in the artifact - a short summary naming the artifact. "+
		"A reply cut off by the model's output limit is continued automatically; never restart from the top.", strings.Join(kinds, ", "))
}

// NewWriteArtifactTool: blob kinds only (structured ones use write_<kind>). hint is the session's
// DocumentHint for hint-requiring kinds, never a tool argument.
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
			// Only hint-requiring kinds get hint; text/bytes derive ids from content, or every write shares one id.
			blobHint := ""
			if ok && spec.RequiresHint {
				blobHint = hint
			}
			id, rev, err := c.SaveBlob(ctx, a.Kind, data, a.Mime, blobHint, lineage)
			if err != nil {
				if msg, ok := artifactschema.RefusalFromError(err); ok {
					return "", errors.New(msg)
				}
				return "", fmt.Errorf("write_artifact: %w", err)
			}
			return fmt.Sprintf("ok: id=%s revision=%d", id, rev), nil
		},
	)
}

// NewWriteKindTool's input schema is spec's registered JSONSchema, not reflected from a Go struct.
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
				if msg, ok := artifactschema.RefusalFromError(err); ok {
					return "", errors.New(msg)
				}
				return "", fmt.Errorf("write_%s: %w", kind, err)
			}
			return fmt.Sprintf("ok: id=%s revision=%d", id, rev), nil
		},
	)
}

// NewWriteKindTools surfaces a schema failure rather than skipping the kind, so the two surfaces can't drift.
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

// readArtifactArgs is id-addressed (from list_artifacts), unlike the MCP's filename-addressed variant.
type readArtifactArgs struct {
	ID       string `json:"id"`
	Revision int    `json:"revision,omitempty"`
	// Offset/Lines window a large text artifact; a window bypasses InlineMaxBytes.
	Offset int `json:"offset,omitempty"`
	Lines  int `json:"lines,omitempty"`
}

// NewReadArtifactTool: an un-windowed web_page/bytes read is cut at fetchReturnMaxBytes, like the judge's.
func NewReadArtifactTool(c *recordstore.Client) (tool.Tool, error) {
	return functiontool.New[readArtifactArgs, string](
		functiontool.Config{
			Name: "read_artifact",
			Description: fmt.Sprintf("Read an artifact by id (from list_artifacts). Text content is returned inline "+
				"(a web_page or bytes artifact up to its first %d bytes); binary content is base64-encoded. Omit revision "+
				"for the latest. Pass offset (a 1-based line number) and/or lines (window size) to read a window of a large "+
				"text artifact instead.", fetchReturnMaxBytes),
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
			// prov sits outside the counted lines an offset/grep hit indexes.
			prov := provenanceHeader(lineage, data)
			if prov != "" {
				prov += "\n\n"
			}
			return prov + shapeReadArtifact(data, mime, a), nil
		},
	)
}

// shapeReadArtifact: windowed, too-large refusal, or whole (base64 if binary).
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
	if isText && sourceKind(a.ID) && len(data) > fetchReturnMaxBytes {
		return capAtLine(string(data))
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

type grepArtifactsArgs struct {
	Pattern string   `json:"pattern"`
	IDs     []string `json:"ids,omitempty"`
}

func newGrepArtifacts(d Deps) (tool.Tool, error) {
	return NewGrepArtifactsTool(d.RecordStore)
}

// NewGrepArtifactsTool: a nil c builds fine but errors on a call.
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

// grepArtifactIDs caps hits at fetchGrepMaxLines across all ids.
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
			// One provenance line per id, outside the line count so hit line numbers stay exact.
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

// BuildNativeArtifactTools: blobHint (DocumentHint) and structuredHint (SubjectHint) differ because
// each kind's save path looks up its own id.

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

// IsNativeArtifactTool reports whether name is built by BuildNativeArtifactTools
// rather than the registry, so a tools: entry naming it only opts the agent in.
func IsNativeArtifactTool(name string) bool {
	switch name {
	case "list_artifacts", "read_artifact", "edit_artifact", "write_artifact":
		return true
	}
	kind, ok := strings.CutPrefix(name, "write_")
	if !ok {
		return false
	}
	spec, ok := recordstore.SpecFor(kind)
	return ok && spec.AgentWritable
}

// SelectArtifactTools always keeps the read tools (web_fetch stubs point at them); a card artifact kind
// implies write_artifact/edit_artifact, its only delivery path.
func SelectArtifactTools(all []tool.Tool, configured []string, artifactKind bool) []tool.Tool {
	want := map[string]bool{"list_artifacts": true, "read_artifact": true, "write_artifact": artifactKind, "edit_artifact": artifactKind}
	for _, n := range configured {
		want[n] = true
	}
	out := make([]tool.Tool, 0, len(all))
	for _, t := range all {
		if want[t.Name()] {
			out = append(out, t)
		}
	}
	return out
}

// sourceKind: fetched pages and dispatch inputs, read for reference - capped when read whole.
// Structured and card artifacts are read to be rewritten, so they keep the full inline limit.
func sourceKind(id string) bool {
	k := recordstore.KindOf(id)
	return k == kindWebPage || k == "bytes"
}

// capAtLine returns text's first fetchReturnMaxBytes cut back to a line boundary, so the offset it
// names to resume from is exact; a line longer than half the cap is cut mid-line instead.
func capAtLine(text string) string {
	head := strings.ToValidUTF8(text[:fetchReturnMaxBytes], "")
	note := ""
	if i := strings.LastIndexByte(head, '\n'); i > fetchReturnMaxBytes/2 {
		head = head[:i]
	} else {
		note = ", the last one cut"
	}
	shown := strings.Count(head, "\n") + 1
	return fmt.Sprintf("%s\n[…lines 1-%d of %d%s (%d of %d bytes); offset=%d to read further, or grep_artifacts to search]",
		head, shown, strings.Count(text, "\n")+1, note, len(head), len(text), shown+1)
}
