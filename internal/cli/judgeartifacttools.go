package cli

import (
	"fmt"
	"strings"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/schema"
)

// RESTArtifactTools builds list_artifacts/read_artifact over c's REST API,
// scoped to chatID - the judge's artifact access when replaying --from-server.
func RESTArtifactTools(c *Client, chatID string) ([]tool.Tool, error) {
	list, err := functiontool.New[restListArtifactsArgs, string](
		functiontool.Config{
			Name:        "list_artifacts",
			Description: "List this chat's artifacts (name, kind, latest revision), optionally filtered by kind.",
		},
		func(ctx agent.Context, a restListArtifactsArgs) (string, error) {
			items, err := c.ListChatArtifacts(ctx, chatID)
			if err != nil {
				return "", fmt.Errorf("list_artifacts: %w", err)
			}
			return formatArtifactSummaries(items, a.Kind), nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("judge replay: list_artifacts: %w", err)
	}
	read, err := functiontool.New[restReadArtifactArgs, string](
		functiontool.Config{
			Name:        "read_artifact",
			Description: "Read an artifact by name (from list_artifacts). Omit revision for the latest.",
		},
		func(ctx agent.Context, a restReadArtifactArgs) (string, error) {
			body, err := c.FetchArtifact(ctx, chatID, a.Name, a.Revision)
			if err != nil {
				return "", fmt.Errorf("read_artifact: %w", err)
			}
			return string(body), nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("judge replay: read_artifact: %w", err)
	}
	return []tool.Tool{list, read}, nil
}

type restListArtifactsArgs struct {
	Kind string `json:"kind,omitempty"`
}

type restReadArtifactArgs struct {
	Name     string `json:"name"`
	Revision int    `json:"revision,omitempty"`
}

// formatArtifactSummaries renders items as one line each, filtered to kind
// when set - the same table shape vetting's own judge tool prints.
func formatArtifactSummaries(items []schema.ArtifactSummary, kind string) string {
	var b strings.Builder
	for _, it := range items {
		k := ""
		if it.Kind != nil {
			k = *it.Kind
		}
		if kind != "" && k != kind {
			continue
		}
		rev := int64(0)
		if it.LatestRevision != nil {
			rev = *it.LatestRevision
		}
		fmt.Fprintf(&b, "%s\trevision=%d\tkind=%s\n", it.Name, rev, k)
	}
	if b.Len() == 0 {
		return "(no artifacts)"
	}
	return b.String()
}
