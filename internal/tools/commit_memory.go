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

// commitMemoryArgs is one durable fact the orchestrator wants to remember.
type commitMemoryArgs struct {
	Content string `json:"content"`
	Kind    string `json:"kind"`
}

// NewCommitMemoryTool builds the orchestrator's commit_memory tool, bound to a
// memory store and the current user. Unlike the gated task path (agents stage,
// the trust gate commits on a judge pass), the orchestrator writes directly: user
// facts are grounded in what the user said, so there is no judge to clear. The
// write still runs through Commit's vet + consolidation (so a changed fact updates
// rather than duplicates).
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
			// Bound the consolidation round-trip so a stalled model can't hang the
			// orchestrator's turn (this write is synchronous - the model awaits it).
			cctx, cancel := context.WithTimeout(ctx, 60*time.Second)
			defer cancel()
			// The user bucket, always: this tool records facts ABOUT THE USER. Legacy is
			// the raw user id - the pre-bucket key these memories used to be written
			// under - so the ones already stored keep loading.
			sc := memory.Scope{User: userID, Legacy: userID}
			if _, err := store.Commit(cctx, sc, "orchestrator", []memory.Candidate{cand}, ""); err != nil {
				return "", fmt.Errorf("commit_memory: %w", err)
			}
			return "Remembered.", nil
		},
	)
}
