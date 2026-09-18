// Package tools registry.
package tools

import (
	"fmt"
	"net/http"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/httpx"
	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/memory"
	"github.com/fagerbergj/quack/internal/recordstore"
	"github.com/fagerbergj/quack/internal/workspace"
)

// Deps: shared dependencies for built-in tools.
type Deps struct {
	Client          *http.Client
	Guarded         *http.Client
	WebSearch       Backend
	Fetch           Backend
	Summarizer      model.LLM
	Cache           *URLCache
	Advisor         adkagent.Agent
	Sessions        session.Service
	Workspace       *workspace.Jail
	WorkspaceUserID string
	WorkspaceCaps   workspace.Caps
	GitCredentials  []GitCredential
	GitTokenSource  GitTokenSource
	Guards          map[string]string
	SafetyJudge     SafetyJudge
	NodeCancelled   func(chatID, nodeID string) bool
	// RepeatGuardTripped ends a node's round when the repeat guard's hard
	// stop fires (dag.Executor.RepeatGuardTripped) - nil leaves only the
	// soft refusal (test builds, no live executor).
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
	"ask_advisor":    func(d Deps) (tool.Tool, error) { return NewAskAdvisorTool(d.Advisor, d.Sessions) },
	"read_file":      newReadFile,
	"list_dir":       newListDir,
	"glob":           newGlob,
	"grep":           newGrep,
	"check_mermaid":  newCheckMermaid,
	"grep_artifacts": newGrepArtifacts,
}

// Build: resolves tool names to ADK tools.
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

// buildOneTool: resolve one name (builtin registry, extension-provided, or error),
// then apply the wrapper chain: scrub, guard, repeat, cancel, emit.
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
		t = et
	} else {
		return nil, fmt.Errorf("tools: unknown builtin tool %q", name)
	}
	t = scrub(t)
	tier, guarded := parseGuardTier(d.Guards[name])

	direct := t
	if guarded {
		if direct, err = newGuardedTool(direct, tier, d.SafetyJudge, d.Sessions); err != nil {
			return nil, fmt.Errorf("tools: guard %q: %w", name, err)
		}
	}
	if direct, err = repeatWrap(direct, repeats, d.RepeatGuardTripped); err != nil {
		return nil, fmt.Errorf("tools: repeat guard %q: %w", name, err)
	}
	if direct, err = cancelWrap(direct, name, d); err != nil {
		return nil, err
	}
	if direct, err = emitWrap(direct, d.LedgerCoords); err != nil {
		return nil, fmt.Errorf("tools: emit wrap %q: %w", name, err)
	}
	return direct, nil
}

// workspaceScrub: respells workspace paths in errors. Identity when no workspace.
func workspaceScrub(d Deps) func(tool.Tool) tool.Tool {
	b, err := newFSBinding(d)
	if err != nil {
		return func(t tool.Tool) tool.Tool { return t }
	}
	return func(t tool.Tool) tool.Tool { return newPathScrub(t, b) }
}

// cancelWrap: outermost wrapper - refused before guard ladder or script statement.
func cancelWrap(t tool.Tool, name string, d Deps) (tool.Tool, error) {
	if d.NodeCancelled == nil {
		return t, nil
	}
	wrapped, err := newCancelGuard(t, d.NodeCancelled)
	if err != nil {
		return nil, fmt.Errorf("tools: cancel guard %q: %w", name, err)
	}
	return wrapped, nil
}
