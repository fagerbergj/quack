package tools

import (
	"context"
	"fmt"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/memory"
)

// commitMemoryArgs: one durable fact to remember.
type commitMemoryArgs struct {
	Content string `json:"content"`
	Kind    string `json:"kind"`
}

// NewCommitMemoryTool: orchestrator's commit_memory tool - writes directly (no judge gate).
func NewCommitMemoryTool(store *memory.Store, userID string) (tool.Tool, error) {
	return functiontool.New[commitMemoryArgs, string](
		functiontool.Config{
			Name: "commit_memory",
			Description: "Remember a durable fact ABOUT THE USER for future conversations - who they are, a " +
				"preference, a relationship, a possession, a goal, or a hard limit. `content` is one atomic " +
				"sentence; `kind` is one of identity|preference|relationship|possession|goal|limit. Only record " +
				"what the user actually told you; skip transient details and anything sensitive they didn't ask " +
				"you to keep.",
		},
		func(ctx agent.Context, a commitMemoryArgs) (string, error) {
			if strings.TrimSpace(a.Content) == "" {
				return "", fmt.Errorf("commit_memory: content is empty")
			}
			cand := memory.Candidate{Content: strings.TrimSpace(a.Content)}
			if a.Kind != "" {
				cand.Metadata = map[string]string{"kind": a.Kind}
			}
			// Bound round-trip so a stalled model can't hang the orchestrator.
			cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			// User bucket: records facts about the user. Legacy is the pre-bucket key for existing memories.
			sc := memory.Scope{User: userID, Legacy: userID}
			if _, err := store.Commit(cctx, sc, "orchestrator", []memory.Candidate{cand}, ""); err != nil {
				return "", fmt.Errorf("commit_memory: %w", err)
			}
			return "Remembered.", nil
		},
	)
}
