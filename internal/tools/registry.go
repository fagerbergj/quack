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
	"github.com/fagerbergj/quack/internal/replay"
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
	ExtTools        map[string]tool.Tool
	Replayer        *replay.Session
	LedgerCoords    ledger.Coords
}

// constructor builds one tool from Deps.
type constructor func(Deps) (tool.Tool, error)

var registry = map[string]constructor{
	"web_search":    newWebSearch,
	"web_fetch":     newFetch,
	"summarize":     newSummarize,
	"current_date":  newCurrentDate,
	"stage_memory":  newStageMemory,
	"ask_user":      func(Deps) (tool.Tool, error) { return NewAskUserTool() },
	"ask_advisor":   func(d Deps) (tool.Tool, error) { return NewAskAdvisorTool(d.Advisor, d.Sessions) },
	"read_file":     newReadFile,
	"list_dir":      newListDir,
	"glob":          newGlob,
	"grep":          newGrep,
	"check_mermaid": newCheckMermaid,
}

// Build: resolves tool names to ADK tools.
func Build(names []string, d Deps) ([]tool.Tool, error) {
	if d.Replayer != nil {
		if d.Replayer.Mode() != replay.ModeFork {
			return newReplayStubs(names, d.Replayer, d.LedgerCoords), nil
		}
		// Fork-replay: build real tools for stub fallback on divergence.
		live := d
		live.Replayer = nil
		liveTools, err := Build(names, live)
		if err != nil {
			return nil, fmt.Errorf("tools: fork-mode live build: %w", err)
		}
		return newReplayStubsWithLive(names, d.Replayer, d.LedgerCoords, liveTools), nil
	}
	if d.Client == nil {
		d.Client = &http.Client{Timeout: 30 * time.Second, Transport: httpx.NewTransport(nil)}
	}
	if d.Guarded == nil {
		d.Guarded = GuardedClient()
	}
	repeats := newRepeatStates()
	scrub := workspaceScrub(d)
	out := make([]tool.Tool, 0, len(names))
	for _, name := range names {
		var t tool.Tool
		var err error
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
		if direct, err = repeatWrap(direct, repeats); err != nil {
			return nil, fmt.Errorf("tools: repeat guard %q: %w", name, err)
		}
		if direct, err = cancelWrap(direct, name, d); err != nil {
			return nil, err
		}
		if direct, err = emitWrap(direct, d.LedgerCoords); err != nil {
			return nil, fmt.Errorf("tools: emit wrap %q: %w", name, err)
		}
		out = append(out, direct)
	}
	return out, nil
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
