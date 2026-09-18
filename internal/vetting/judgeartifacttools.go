// judgeartifacttools.go: judge's own list_artifacts/read_artifact, scoped to
// the node's chat via the same recordstore.Client the worker's tools use.
package vetting

import (
	"fmt"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/recordstore"
)

// judgeArtifactReadCap bounds one read_artifact reply so a large stored
// artifact can't blow the judge's own prompt budget.
const judgeArtifactReadCap = 24_000

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

type judgeReadArtifactArgs struct {
	ID       string `json:"id"`
	Revision int    `json:"revision,omitempty"`
}

func newJudgeReadArtifactTool(c *recordstore.Client) (tool.Tool, error) {
	return functiontool.New[judgeReadArtifactArgs, string](
		functiontool.Config{
			Name: "read_artifact",
			Description: "Read an artifact by id (from list_artifacts). Omit revision for the latest, or pass one " +
				"to read exactly what the worker wrote/edited at that point.",
		},
		func(ctx agent.Context, a judgeReadArtifactArgs) (string, error) {
			var data []byte
			var ok bool
			var err error
			if a.Revision > 0 {
				data, ok, err = c.LoadVersion(ctx, a.ID, a.Revision)
			} else {
				data, _, _, _, ok, err = c.LatestWithMeta(ctx, a.ID)
			}
			if err != nil {
				return "", fmt.Errorf("read_artifact: %w", err)
			}
			if !ok {
				return "", fmt.Errorf("read_artifact: %s: not found", a.ID)
			}
			return boundExcerpt(string(data), judgeArtifactReadCap), nil
		},
	)
}
