package tools

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/httpx"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/stream"
	"github.com/fagerbergj/quack/internal/workspace"
)

type Deps struct {
	Client          *http.Client
	Guarded         *http.Client
	WebSearch       Backend
	Fetch           Backend
	Summarizer      model.LLM
	Cache           *URLCache
	Sessions        session.Service
	Workspace       *workspace.Jail
	WorkspaceUserID string
	WorkspaceCaps   workspace.Caps
	Guards          map[string]string
	SafetyJudge     SafetyJudge
	NodeCancelled   func(chatID, nodeID string) bool
	// RepeatGuardTripped ends a node's round on the repeat guard's hard stop; nil leaves only the soft refusal.
	RepeatGuardTripped func(chatID, nodeID, msg string) bool
	ExtTools           map[string]tool.Tool
	LedgerCoords       ledger.Coords
	Memory             *memory.Store      // recall_memory/load_memory (nil = not offered - see resolveToolNames)
	MemoryRole         string             // recall_memory/load_memory's role bucket; empty falls back to repo then user
	Ledger             ledger.LedgerStore // recall_memory/load_memory's memory.recall ledger entries
	// Repeats lets a caller share this call's repeat-guard state with tools it wraps
	// outside Build (e.g. RepeatWrapToolset); nil creates a fresh instance.
	Repeats *repeatStates
	// RecordStore/NodeID/Coords back web_fetch's large-page storage and
	// grep_artifacts; nil RecordStore degrades both (no chat context).
	RecordStore *recordstore.Client
	NodeID      string
	Coords      *RoundCoords
	// Sink/TurnID: a node's chat SSE sink and turn id, which its tools' ctx lacks behind the A2A boundary.
	Sink   func(stream.SSEEvent)
	TurnID string
	// CallScope: the DAG node this build serves - fs, memory and guard scoping plus
	// extension sdk.CallInfo resolve from it, never from prompt text; zero outside a node.
	CallScope CallScope
}

type constructor func(Deps) (tool.Tool, error)

var registry = map[string]constructor{
	"web_search":     newWebSearch,
	"web_fetch":      newFetch,
	"summarize":      newSummarize,
	"current_date":   newCurrentDate,
	"stage_memory":   newStageMemory,
	"recall_memory":  newRecallMemory,
	"load_memory":    newLoadMemory,
	"ask_user":       func(Deps) (tool.Tool, error) { return NewAskUserTool() },
	"read_file":      newReadFile,
	"list_dir":       newListDir,
	"glob":           newGlob,
	"grep":           newGrep,
	"check_mermaid":  newCheckMermaid,
	"render_ui":      newRenderUI,
	"grep_artifacts": newGrepArtifacts,
	"weather":        newWeather,
}

// ErrUnknownTool: a tools: entry no builtin or enabled extension provides.
var ErrUnknownTool = errors.New("unknown tool")

func Build(names []string, d Deps) ([]tool.Tool, error) {
	if d.Client == nil {
		d.Client = &http.Client{Timeout: 30 * time.Second, Transport: httpx.NewTransport(nil)}
	}
	if d.Guarded == nil {
		d.Guarded = GuardedClient()
	}
	repeats := d.Repeats
	if repeats == nil {
		repeats = newRepeatStates()
	}
	scrub := workspaceScrub(d)
	out := make([]tool.Tool, 0, len(names))
	for _, name := range names {
		t, err := buildOneTool(name, d, repeats, scrub)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// buildOneTool wraps innermost to outermost: scrub, guard, repeat, cancel, emit.
func buildOneTool(name string, d Deps, repeats *repeatStates, scrub func(tool.Tool) tool.Tool) (tool.Tool, error) {
	var (
		t   tool.Tool
		err error
	)
	if ctor, ok := registry[name]; ok {
		if t, err = ctor(d); err != nil {
			return nil, fmt.Errorf("tools: build %q: %w", name, err)
		}
	} else if et, ok := d.ExtTools[name]; ok {
		if et == nil {
			return nil, fmt.Errorf("tools: tool name %q is provided by more than one extension; use its <plugin>_%s prefixed form", name, name)
		}
		t = withCallInfo(et, d)
	} else {
		return nil, fmt.Errorf("tools: unknown builtin tool %q: %w", name, ErrUnknownTool)
	}
	t = scrub(t)
	tier, guarded := parseGuardTier(d.Guards[name])

	direct := t
	if guarded {
		if direct, err = newGuardedTool(direct, tier, d.SafetyJudge, d.Sessions, d.CallScope); err != nil {
			return nil, fmt.Errorf("tools: guard %q: %w", name, err)
		}
	}
	if direct, err = repeatWrap(direct, repeats, d.RepeatGuardTripped, d.CallScope); err != nil {
		return nil, fmt.Errorf("tools: repeat guard %q: %w", name, err)
	}
	if direct, err = cancelWrap(direct, name, d); err != nil {
		return nil, err
	}
	return emitWrap(direct, d.LedgerCoords), nil
}

// workspaceScrub: respells workspace paths in errors. Identity when no workspace.
func workspaceScrub(d Deps) func(tool.Tool) tool.Tool {
	b, err := newFSBinding(d)
	if err != nil {
		return func(t tool.Tool) tool.Tool { return t }
	}
	return func(t tool.Tool) tool.Tool { return newPathScrub(t, b) }
}

// cancelWrap: a cancelled node is refused before the guard ladder runs.
func cancelWrap(t tool.Tool, name string, d Deps) (tool.Tool, error) {
	if d.NodeCancelled == nil {
		return t, nil
	}
	wrapped, err := newCancelGuard(t, d.NodeCancelled, d.CallScope)
	if err != nil {
		return nil, fmt.Errorf("tools: cancel guard %q: %w", name, err)
	}
	return wrapped, nil
}
