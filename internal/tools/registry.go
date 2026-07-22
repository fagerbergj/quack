// Package tools is Quack's registry of built-in agent tools (config `kind:
// builtin`). An agent's config selects tools by name; Build resolves those names
// to ADK tools, injecting shared dependencies (HTTP clients, backend URLs, a
// summarizer model).
package tools

import (
	"fmt"
	"net/http"
	"time"

	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/model"
	"google.golang.org/adk/v2/session"
	"google.golang.org/adk/v2/tool"

	"github.com/fagerbergj/quack/internal/workspace"
)

// Deps are the shared dependencies injected into built-in tools.
type Deps struct {
	// Client is a plain HTTP client for trusted internal backends that live on
	// private network addresses.
	Client *http.Client
	// Guarded is an SSRF-protected HTTP client for fetching untrusted,
	// agent-chosen URLs. Defaults to GuardedClient().
	Guarded *http.Client
	// WebSearch binds the web_search tool's backend (kind + endpoint).
	WebSearch Backend
	// Fetch binds the web_fetch tool's render backend (kind + endpoint).
	// Optional; an empty URL means fetch skips the render fallback.
	Fetch Backend
	// Summarizer is the model the summarize tool uses to condense text.
	Summarizer model.LLM
	// Cache is a shared response cache used by web_fetch and web_search to avoid
	// redundant network requests. Optional; when nil, caching is disabled.
	Cache *URLCache
	// Advisor is the mentor agent ask_advisor consults. nil ⇒ the tool is never
	// registered (build skips it; see resolveToolNames in internal/serve).
	Advisor adkagent.Agent
	// Sessions is the shared session.Service ask_advisor uses to persist each
	// node's mentor conversation (same store as chat/plan sessions, a distinct
	// AppName - see ask_advisor.go).
	Sessions session.Service
	// Workspace is the filesystem jail the fs.go read tools (read_file,
	// list_dir, glob, grep - the judge's inspection surface) resolve every
	// path through. nil ⇒ those tools are never registered (build errors
	// clearly if an agent's tools: list requests one anyway).
	Workspace *workspace.Jail
	// WorkspaceUserID is the identity fs tools are bound to at construction
	// (the isolation model's "no identity parsing inside tool handlers" rule).
	// Quack is single-user today, so this is always the "local" constant
	// (internal/server/rest and internal/server/mcp use the same constant).
	WorkspaceUserID string
	// WorkspaceCaps bounds fs tool calls (read/write bytes, grep/glob/list
	// result caps). Zero value ⇒ workspace.DefaultCaps().
	WorkspaceCaps workspace.Caps
	// GitCredentials are the deployment-level per-host HTTPS credentials git
	// tools authenticate with (workspace.git_credentials). Empty ⇒ git
	// operations proceed unauthenticated (public repos only).
	GitCredentials []GitCredential
	// GitTokenSource is an optional dynamic per-host credential source consulted
	// when no static GitCredentials entry matches - the extension seam (e.g. a
	// GitHub App installation token). nil ⇒ static credentials only.
	GitTokenSource GitTokenSource
	// Guards maps a tool name to its guard tier (workspace.guards: none |
	// judge | confirm | judge+confirm). A tool with no entry is unguarded
	// (Tier 0 walls - the fs/git path jail, the OS sandbox around child
	// processes - still always apply).
	Guards map[string]string
	// SafetyJudge backs the guard ladder's judge tier: an independent model
	// call that allows/denies a proposed operation. nil ⇒ a tool configured
	// for a judge tier fails closed at build time (see buildGuarded).
	SafetyJudge SafetyJudge
	// NodeCancelled reports whether the DAG node a call runs for was cancelled
	// by the user (dag.Executor.NodeCancelled, wired in internal/serve). Build
	// wraps EVERY tool in the cancel guard when it is set, so a cancelled
	// worker stops at its next TOOL CALL rather than grinding on to the gate's
	// next stage boundary (see cancelguard.go). nil ⇒ no guard (an un-gated
	// tool build, e.g. the judge's read tools).
	NodeCancelled func(chatID, nodeID string) bool
	// ExtTools are extension-supplied tools (e.g. the GitHub App's
	// github_add_review_comment - internal/github.App.Tools()), keyed by name.
	// An agent gets one ONLY if its config tools: list names it - same
	// resolution path as every builtin, so an extension makes a tool AVAILABLE,
	// never force-injects it (see registry lookup in Build). Empty/nil when no
	// extension is configured.
	ExtTools map[string]tool.Tool
}

// constructor builds one tool from the shared dependencies.
type constructor func(Deps) (tool.Tool, error)

var registry = map[string]constructor{
	"web_search":   newWebSearch,
	"web_fetch":    newFetch,
	"summarize":    newSummarize,
	"current_date": newCurrentDate,
	"stage_memory": newStageMemory,
	"ask_user":     func(Deps) (tool.Tool, error) { return NewAskUserTool() },
	"ask_advisor":  func(d Deps) (tool.Tool, error) { return NewAskAdvisorTool(d.Advisor, d.Sessions) },
	// Filesystem tools (internal/tools/fs.go), all bound to (userID, jail) -
	// see fsBinding / newFSBinding.
	"read_file": newReadFile,
	"list_dir":  newListDir,
	"glob":      newGlob,
	"grep":      newGrep,
}

// Build resolves tool names to ADK tools, injecting d. A name resolves against
// the static registry above first, then d.ExtTools (an extension's tools,
// keyed by name - e.g. github_add_review_comment) - the SAME resolution path,
// so an extension only reaches an agent that lists its tool by name; nothing
// is force-injected. A name in neither is an error.
func Build(names []string, d Deps) ([]tool.Tool, error) {
	if d.Client == nil {
		d.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if d.Guarded == nil {
		d.Guarded = GuardedClient()
	}
	// One repeat-state per build (= per agent): consecutive identical calls are
	// judged across ALL of the agent's tools within a session (see repeatguard.go).
	repeats := newRepeatStates()
	// Every tool is wrapped at the ONE construction point: host-path scrubbed
	// innermost (hostpath.go - an error carrying a resolved path comes from
	// os/git, not the tool, so every tool would leak it), then the guard ladder
	// (guard.go), then the repeat/cancel guards.
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
		out = append(out, direct)
	}
	return out, nil
}

// workspaceScrub returns the INNERMOST wrapper both views of a tool are built
// through: the host-path scrub (hostpath.go), which respells any workspace path in
// a returned ERROR in the model's own namespace. Innermost, so a script's recorded
// call error is scrubbed too, and so nothing a tool reports can slip past it.
//
// Identity, not a no-op, when there is no workspace configured (a web-only agent
// has no jail, so there is no host path of ours to leak).
func workspaceScrub(d Deps) func(tool.Tool) tool.Tool {
	b, err := newFSBinding(d)
	if err != nil {
		return func(t tool.Tool) tool.Tool { return t }
	}
	return func(t tool.Tool) tool.Tool { return newPathScrub(t, b) }
}

// cancelWrap applies the per-node cancel guard (cancelguard.go). It is the
// OUTERMOST wrapper on both views of a tool: a cancelled node's call is refused
// before it can reach the guard ladder (no point safety-judging or human-confirming
// an operation for a node the user just stopped), and before it can reach a script's
// next statement (a cancelled node must stop mid-script).
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
