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
	// ID/Revision/Offset/Lines mirror vetting's own read_artifact tool shape;
	// offset/lines window client-side since the REST endpoint returns the whole body.
	read, err := functiontool.New[restReadArtifactArgs, string](
		functiontool.Config{
			Name: "read_artifact",
			Description: "Read an artifact by id (from list_artifacts). Omit revision for the latest. " +
				"Pass offset (a 1-based line number) and/or lines to window a large text artifact.",
		},
		func(ctx agent.Context, a restReadArtifactArgs) (string, error) {
			body, err := c.FetchArtifact(ctx, chatID, a.ID, a.Revision)
			if err != nil {
				return "", fmt.Errorf("read_artifact: %w", err)
			}
			return windowLines(string(body), a.Offset, a.Lines), nil
		},
	)
	if err != nil {
		return nil, fmt.Errorf("judge replay: read_artifact: %w", err)
	}
	return []tool.Tool{list, read}, nil
}

// StubArtifactTools builds list_artifacts/read_artifact that both answer "no
// artifacts available" - the judge prompt always advertises these tools, so they must exist.
func StubArtifactTools() ([]tool.Tool, error) {
	const notAvailable = "no artifacts are available: this round is replaying from a local bundle, not a live server"
	list, err := functiontool.New[restListArtifactsArgs, string](
		functiontool.Config{Name: "list_artifacts", Description: "List this chat's artifacts."},
		func(agent.Context, restListArtifactsArgs) (string, error) { return notAvailable, nil },
	)
	if err != nil {
		return nil, fmt.Errorf("judge replay: list_artifacts stub: %w", err)
	}
	read, err := functiontool.New[restReadArtifactArgs, string](
		functiontool.Config{Name: "read_artifact", Description: "Read an artifact by id."},
		func(agent.Context, restReadArtifactArgs) (string, error) { return notAvailable, nil },
	)
	if err != nil {
		return nil, fmt.Errorf("judge replay: read_artifact stub: %w", err)
	}
	return []tool.Tool{list, read}, nil
}

type restListArtifactsArgs struct {
	Kind string `json:"kind,omitempty"`
}

type restReadArtifactArgs struct {
	ID       string `json:"id"`
	Revision int    `json:"revision,omitempty"`
	Offset   int    `json:"offset,omitempty"`
	Lines    int    `json:"lines,omitempty"`
}

// windowLines returns body's lines [offset, offset+lines), 1-based; offset<=0
// or lines<=0 leaves that bound open.
func windowLines(body string, offset, lines int) string {
	if offset <= 0 && lines <= 0 {
		return body
	}
	all := strings.Split(body, "\n")
	start := offset - 1
	if start < 0 {
		start = 0
	}
	if start >= len(all) {
		return ""
	}
	end := len(all)
	if lines > 0 && start+lines < end {
		end = start + lines
	}
	return strings.Join(all[start:end], "\n")
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
