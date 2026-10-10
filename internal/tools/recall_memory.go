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

type recallMemoryArgs struct {
	Query string `json:"query"`
	K     int    `json:"k,omitempty"`
}

// recallMemoryResult: Truncated reports hits the injection byte budget dropped.
type recallMemoryResult struct {
	Hits      []memory.Delivered `json:"hits"`
	Truncated bool               `json:"truncated,omitempty"`
}

// recallMemoryDescription is shared by every recall_memory surface so the guidance never differs.
const recallMemoryDescription = "Recall up to k durable facts from shared memory relevant to `query`, scoped to your " +
	"own buckets (repo/role/user). Returns a compact list of {id, tier, score, content} - cite an id in your answer " +
	"when you rely on it. Every call is logged and the delivered memories may be voted on by the judge."

// NewRecallMemoryTool: a fixed scope for the orchestrator, rebuilt every turn so counted dedupes per turn.
func NewRecallMemoryTool(store *memory.Store, sc memory.Scope, led ledger.LedgerStore, chatID string) (tool.Tool, error) {
	var mu sync.Mutex
	counted := map[string]bool{}
	return functiontool.New[recallMemoryArgs, recallMemoryResult](
		functiontool.Config{Name: "recall_memory", Description: recallMemoryDescription},
		func(ctx agent.Context, a recallMemoryArgs) (recallMemoryResult, error) {
			if strings.TrimSpace(a.Query) == "" {
				return recallMemoryResult{}, fmt.Errorf("recall_memory: query is empty")
			}
			hits, truncated := store.RecallForTool(ctx, sc, a.Query, a.K)
			store.LogRecallLedgerOnly(ctx, led, chatID, "", "tool", hits)
			mu.Lock()
			newIDs := make([]string, 0, len(hits))
			for _, h := range hits {
				if counted[h.ID] {
					continue
				}
				counted[h.ID] = true
				newIDs = append(newIDs, h.ID)
			}
			mu.Unlock()
			store.RecordRecall(ctx, newIDs)
			return recallMemoryResult{Hits: hits, Truncated: truncated}, nil
		},
	)
}

// coordsBox: re-stamped per dispatch, since the tool is built before nodeID is known and resolves
// memory.Scope from these coords.
type coordsBox struct {
	mu     sync.Mutex
	coords ledger.Coords
}

func (b *coordsBox) set(c ledger.Coords) { b.mu.Lock(); b.coords = c; b.mu.Unlock() }
func (b *coordsBox) get() ledger.Coords  { b.mu.Lock(); defer b.mu.Unlock(); return b.coords }

// recallMemoryTool implements ledger.CoordSetter, so StampCoords restamps it with each dispatch's
// ChatID/Node before the node runs.
type recallMemoryTool struct {
	runnableTool
	box *coordsBox
}

func (t *recallMemoryTool) SetLedgerCoords(c ledger.Coords) { t.box.set(c) }

// recallScope mirrors vetting.MemoryScope from coords. Never sets Legacy: pre-scope memories were keyed
// by agent name, never by node id.
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

func newRecallMemory(d Deps) (tool.Tool, error) { return newRecallMemoryNamed(d, "recall_memory") }

// newLoadMemory: logged, counted and judge-scanned exactly like recall_memory.
func newLoadMemory(d Deps) (tool.Tool, error) { return newRecallMemoryNamed(d, "load_memory") }

// newRecallMemoryNamed re-derives scope per call from coordsBox, so tools never imports vetting.
// Only the ledger entry lands here; vetting's round merge bumps the counter.
func newRecallMemoryNamed(d Deps, name string) (tool.Tool, error) {
	// No nil guard: Store's methods are nil-receiver safe, so an early resolve builds a no-op tool.
	box := &coordsBox{}
	inner, err := functiontool.New[recallMemoryArgs, recallMemoryResult](
		functiontool.Config{Name: name, Description: recallMemoryDescription},
		func(ctx agent.Context, a recallMemoryArgs) (recallMemoryResult, error) {
			if strings.TrimSpace(a.Query) == "" {
				return recallMemoryResult{}, fmt.Errorf("%s: query is empty", name)
			}
			coords := box.get()
			sc := recallScope(d, ctx, coords)
			hits, truncated := d.Memory.RecallForTool(ctx, sc, a.Query, a.K)
			d.Memory.LogRecallLedgerOnly(ctx, d.Ledger, coords.ChatID, coords.Node, "tool", hits)
			return recallMemoryResult{Hits: hits, Truncated: truncated}, nil
		},
	)
	return wrapRunnable(name, box, inner, err)
}

// wrapRunnable is split out so both failure paths are directly testable.
func wrapRunnable(name string, box *coordsBox, inner tool.Tool, err error) (tool.Tool, error) {
	if err != nil {
		return nil, err
	}
	rt, ok := inner.(runnableTool)
	if !ok {
		return nil, fmt.Errorf("tools: %s: functiontool does not implement runnableTool", name)
	}
	return &recallMemoryTool{runnableTool: rt, box: box}, nil
}
