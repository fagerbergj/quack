// judgeartifacttools.go: judge's own list_artifacts/read_artifact, scoped to
// the node's chat via the same recordstore.Client the worker's tools use.
package vetting

import (
	"encoding/base64"
	"fmt"
	"strings"
	"unicode/utf8"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/artifactref"
	"github.com/fagerbergj/quack/internal/recordstore"
)

// judgeArtifactReadCap bounds one read_artifact reply so a large stored
// artifact can't blow the judge's own prompt budget.
const judgeArtifactReadCap = 24_000

// judgeHiddenKinds: gate-owned records excluded from list_artifacts - a judge
// reading its own prior verdict/delivery decisions as "evidence" is circular.
var judgeHiddenKinds = map[string]bool{kindJudgeRound: true, kindDeliveryRecord: true}

// NewJudgeArtifactTools builds list_artifacts + read_artifact over c (no
// edit/write). Duplicated from internal/tools: tools imports vetting, so the reverse import would cycle.
func NewJudgeArtifactTools(c *recordstore.Client) ([]tool.Tool, error) {
	list, err := newJudgeListArtifactsTool(c)
	if err != nil {
		return nil, fmt.Errorf("vetting: judge list_artifacts: %w", err)
	}
	read, err := newJudgeReadArtifactTool(c)
	if err != nil {
		return nil, fmt.Errorf("vetting: judge read_artifact: %w", err)
	}
	return []tool.Tool{list, read}, nil
}

type judgeListArtifactsArgs struct {
	Kind string `json:"kind,omitempty"`
}

func newJudgeListArtifactsTool(c *recordstore.Client) (tool.Tool, error) {
	return functiontool.New[judgeListArtifactsArgs, string](
		functiontool.Config{
			Name:        "list_artifacts",
			Description: "List this chat's artifacts (id, kind, latest revision, authoring node), optionally filtered by kind.",
		},
		func(ctx agent.Context, a judgeListArtifactsArgs) (string, error) {
			items, err := c.List(ctx, a.Kind)
			if err != nil {
				return "", fmt.Errorf("list_artifacts: %w", err)
			}
			var b strings.Builder
			for _, it := range items {
				if judgeHiddenKinds[it.Kind] {
					continue
				}
				fmt.Fprintf(&b, "%s\trevision=%d\tkind=%s\tnode=%s\n", it.ID, it.Revision, it.Kind, it.NodeID)
			}
			if b.Len() == 0 {
				return "(no artifacts)", nil
			}
			return b.String(), nil
		},
	)
}

// judgeReadArtifactArgs mirrors tools.readArtifactArgs's id/revision/window
// shape (internal/tools/artifacts.go) - the judge needs the same window a
// large artifact requires, not just the worker.
type judgeReadArtifactArgs struct {
	ID       string `json:"id"`
	Revision int    `json:"revision,omitempty"`
	Offset   int    `json:"offset,omitempty"`
	Lines    int    `json:"lines,omitempty"`
}

func newJudgeReadArtifactTool(c *recordstore.Client) (tool.Tool, error) {
	return functiontool.New[judgeReadArtifactArgs, string](
		functiontool.Config{
			Name: "read_artifact",
			Description: "Read an artifact by id (from list_artifacts). Text content is returned inline; binary " +
				"content is base64-encoded. Omit revision for the latest, or pass one to read exactly what the " +
				"worker wrote/edited at that point. Pass offset (a 1-based line number) and/or lines to window a " +
				"large text artifact instead of the whole thing.",
		},
		func(ctx agent.Context, a judgeReadArtifactArgs) (string, error) {
			var data []byte
			var mime string
			var ok bool
			var err error
			if a.Revision > 0 {
				data, ok, err = c.LoadVersion(ctx, a.ID, a.Revision)
			} else {
				data, mime, _, _, ok, err = c.LatestWithMeta(ctx, a.ID)
			}
			if err != nil {
				return "", fmt.Errorf("read_artifact: %w", err)
			}
			if !ok {
				return "", fmt.Errorf("read_artifact: %s: not found", a.ID)
			}
			return shapeJudgeReadArtifact(data, mime, a), nil
		},
	)
}

// judgeArtifactWindowLines caps a windowed read_artifact reply, mirroring
// tools.maxWindowLines.
const judgeArtifactWindowLines = 500

// shapeJudgeReadArtifact: windowed or bounded text, or base64 for binary data
// (mirrors tools.shapeReadArtifact - a judge round hits the same large/binary
// artifacts a worker's own read_artifact call does).
func shapeJudgeReadArtifact(data []byte, mime string, a judgeReadArtifactArgs) string {
	isText := (mime != "" && (strings.HasPrefix(mime, "text/") || mime == "application/json")) ||
		(mime == "" && utf8.Valid(data))
	if !isText {
		if len(data) > artifactref.InlineMaxBytes {
			return fmt.Sprintf("size: %d bytes (exceeds %d byte read_artifact limit)", len(data), artifactref.InlineMaxBytes)
		}
		return "mime: " + mime + "\n\n" + base64.StdEncoding.EncodeToString(data)
	}
	if a.Offset > 0 || a.Lines > 0 {
		lines := strings.Split(string(data), "\n")
		return judgeWindowLines(lines, a.Offset, a.Lines, len(lines))
	}
	return boundExcerpt(string(data), judgeArtifactReadCap)
}

// judgeWindowLines returns lines[start-1:end] (1-based, capped at total and
// at judgeArtifactWindowLines) plus a navigation footer.
func judgeWindowLines(lines []string, offset, want, total int) string {
	start := offset
	if start < 1 {
		start = 1
	}
	if start > total {
		return fmt.Sprintf("[offset %d is past the end of this artifact (%d lines).]", start, total)
	}
	if want <= 0 || want > judgeArtifactWindowLines {
		want = judgeArtifactWindowLines
	}
	end := start + want - 1
	if end > total {
		end = total
	}
	body := strings.Join(lines[start-1:end], "\n")
	if end < total {
		return fmt.Sprintf("%s\n\n[lines %d-%d of %d. offset=%d to read further.]", body, start, end, total, end+1)
	}
	return fmt.Sprintf("%s\n\n[lines %d-%d of %d (end).]", body, start, end, total)
}

// BoundJudgeArtifactRead shapes an artifact body for a judge exactly as the live
// read_artifact tool does (24KB text cap, 500-line windows, base64 for binary).
func BoundJudgeArtifactRead(data []byte, mime string, offset, lines int) string {
	return shapeJudgeReadArtifact(data, mime, judgeReadArtifactArgs{Offset: offset, Lines: lines})
}
