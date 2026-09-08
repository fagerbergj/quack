package tools

import (
	"fmt"
	"strings"
	"sync"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/memory"
)

// recallMemoryArgs: recall_memory(query, k?) (epic #1255 P2).
type recallMemoryArgs struct {
	Query string `json:"query"`
	K     int    `json:"k,omitempty"`
}

// recallMemoryResult: a compact id/tier/score/content list the model can
// cite by id, plus whether the injection byte budget dropped any hits.
type recallMemoryResult struct {
	Hits      []memory.Delivered `json:"hits"`
	Truncated bool               `json:"truncated,omitempty"`
}

// recallMemoryDescription is shared by every recall_memory surface (native
// registry, orchestrator, ACP loopback MCP) so the model sees identical
// guidance regardless of caller.
const recallMemoryDescription = "Recall up to k durable facts from shared memory relevant to `query`, scoped to your " +
	"own buckets (repo/role/user). Returns a compact list of {id, tier, score, content} - cite an id in your answer " +
	"when you rely on it. Every call is logged and the delivered memories may be voted on by the judge."

// NewRecallMemoryTool builds recall_memory over a FIXED scope - the
// orchestrator's own use, which has no per-node coords to restamp (unlike
// newRecallMemory below, built once per agent and re-scoped per dispatch).
func NewRecallMemoryTool(store *memory.Store, sc memory.Scope, led ledger.LedgerStore, chatID string) (tool.Tool, error) {
	return functiontool.New[recallMemoryArgs, recallMemoryResult](
		functiontool.Config{Name: "recall_memory", Description: recallMemoryDescription},
		func(ctx agent.Context, a recallMemoryArgs) (recallMemoryResult, error) {
			if strings.TrimSpace(a.Query) == "" {
				return recallMemoryResult{}, fmt.Errorf("recall_memory: query is empty")
			}
			hits, truncated := store.RecallForTool(ctx, sc, a.Query, a.K)
			store.LogRecall(ctx, led, chatID, "", "tool", hits)
			return recallMemoryResult{Hits: hits, Truncated: truncated}, nil
		},
	)
}

// coordsBox: mutable ledger.Coords a tool built once (before nodeID is
// known) can be re-stamped with per dispatch - same need SetLedgerCoords
// solves for emitTool/guardedTool, extracted here since recallMemoryTool
// also has to resolve memory.Scope from the same coords, not just log them.
type coordsBox struct {
	mu     sync.Mutex
	coords ledger.Coords
}

func (b *coordsBox) set(c ledger.Coords) { b.mu.Lock(); b.coords = c; b.mu.Unlock() }
func (b *coordsBox) get() ledger.Coords  { b.mu.Lock(); defer b.mu.Unlock(); return b.coords }

// recallMemoryTool wraps the functiontool built by newRecallMemory so it can
// also implement ledger.CoordSetter - ledger.StampCoords (dag/graph.go)
// restamps every built-in tool with this dispatch's ChatID/Node right before
// the node runs, the same mechanism artifact tools rely on for identity.
// Embeds runnableTool (not tool.Tool) so Declaration/Run/ProcessRequest -
// needed by the rest of Build's wrapper chain - are promoted too.
type recallMemoryTool struct {
	runnableTool
	box *coordsBox
}

func (t *recallMemoryTool) SetLedgerCoords(c ledger.Coords) { t.box.set(c) }

// recallScope mirrors vetting.MemoryScope (role from the agent bundle, repo
// from the workspace, user from the session) but takes coords directly - the
// mutable-box re-derivation newRecallMemory does per call, pulled out so a
// test can assert the bucket list without wiring a whole ADK tool call.
// Deliberately never sets Legacy: that field is only for pre-scope memories
// keyed by agent NAME, and a node id never had memories under it (#1262/#1263).
func recallScope(d Deps, ctx agent.Context, coords ledger.Coords) memory.Scope {
	sc := memory.Scope{Role: d.MemoryRole}
	if s := ctx.Session(); s != nil {
		sc.User = s.UserID()
	}
	if d.Workspace != nil {
		sc.Repo = d.Workspace.RepoKey(d.WorkspaceUserID, coords.ChatID)
	}
	return sc
}

// newRecallMemory builds the registry's recall_memory for native DAG
// workers. Scope mirrors vetting.MemoryScope (role from the agent bundle,
// repo from the workspace, user from the session) but is re-derived on
// every call from the mutable coords box instead of vetting.Config, so
// internal/tools never has to import internal/vetting.
func newRecallMemory(d Deps) (tool.Tool, error) {
	// No Memory-nil guard: Store's own methods (RecallForTool/LogRecall) are
	// nil-receiver safe, same leniency as stage_memory - a caller resolving
	// tools ahead of the real per-agent Deps (e.g. a grant-check test) must
	// still get a buildable tool, just one that recalls nothing until wired.
	box := &coordsBox{}
	inner, err := functiontool.New[recallMemoryArgs, recallMemoryResult](
		functiontool.Config{Name: "recall_memory", Description: recallMemoryDescription},
		func(ctx agent.Context, a recallMemoryArgs) (recallMemoryResult, error) {
			if strings.TrimSpace(a.Query) == "" {
				return recallMemoryResult{}, fmt.Errorf("recall_memory: query is empty")
			}
			coords := box.get()
			sc := recallScope(d, ctx, coords)
			hits, truncated := d.Memory.RecallForTool(ctx, sc, a.Query, a.K)
			d.Memory.LogRecall(ctx, d.Ledger, coords.ChatID, coords.Node, "tool", hits)
			return recallMemoryResult{Hits: hits, Truncated: truncated}, nil
		},
	)
	if err != nil {
		return nil, err
	}
	rt, ok := inner.(runnableTool)
	if !ok {
		return nil, fmt.Errorf("tools: recall_memory: functiontool does not implement runnableTool")
	}
	return &recallMemoryTool{runnableTool: rt, box: box}, nil
}
