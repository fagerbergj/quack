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

	"github.com/fagerbergj/quack/internal/vetting"
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
	// AppName — see ask_advisor.go).
	Sessions session.Service
	// Workspace is the filesystem jail the fs.go tools (read_file, write_file,
	// edit_file, list_dir, glob, grep, delete_path) resolve every path
	// through. nil ⇒ those tools are never registered (build errors clearly if
	// an agent's tools: list requests one anyway; see internal/serve).
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
	// GitPush gates the git_push tool (workspace.git_push, default false) —
	// the one outward-facing, non-undoable git operation.
	GitPush bool
	// GitTokenSource is an optional dynamic per-host credential source consulted
	// when no static GitCredentials entry matches — the extension seam (e.g. a
	// GitHub App installation token). nil ⇒ static credentials only.
	GitTokenSource GitTokenSource
	// Guards maps a tool name to its guard tier (workspace.guards: none |
	// judge | confirm | judge+confirm). A tool with no entry is unguarded
	// (Tier 0 walls — the fs/git path jail, the OS sandbox around child
	// processes — still always apply).
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
	// Filesystem tools (internal/tools/fs.go), all bound to (userID, jail) —
	// see fsBinding / newFSBinding.
	"read_file":   newReadFile,
	"write_file":  newWriteFile,
	"edit_file":   newEditFile,
	"list_dir":    newListDir,
	"glob":        newGlob,
	"grep":        newGrep,
	"delete_path": newDeletePath,
	// Git tools (internal/tools/git.go), all bound to (userID, jail,
	// credentials, push-enabled) — see gitBinding / newGitBinding.
	"git_clone":           newGitClone,
	"git_checkout":        newGitCheckout,
	"git_status":          newGitStatus,
	"git_diff":            newGitDiff,
	"git_log":             newGitLog,
	"git_commit":          newGitCommit,
	"git_branch":          newGitBranch,
	"git_push":            newGitPush,
	"git_worktree_create": newGitWorktreeCreate,
	"git_worktree_remove": newGitWorktreeRemove,
	"git_pull":            newGitPull,
	"git_rebase":          newGitRebase,
	// run_command (internal/tools/run_command.go), bound to (userID, jail, caps)
	// like the fs tools — executes via the SAME jailed argv runner the trust
	// gate's per-node deterministic checks use (internal/workspace.RunArgv).
	"run_command": newRunCommand,
	// cd (internal/tools/cd.go): sets the session working directory (CwdKey in
	// state) AND loads that location's project context (nearest AGENTS.md/
	// CLAUDE.md + discovered project skills). Read-only-ish context tool, safe
	// for any agent that works in a cloned repo.
	"cd": newCd,
}

// Build resolves tool names to ADK tools, injecting d. Unknown names are an
// error (the extension seam for future tool kinds).
func Build(names []string, d Deps) ([]tool.Tool, error) {
	if d.Client == nil {
		d.Client = &http.Client{Timeout: 30 * time.Second}
	}
	if d.Guarded == nil {
		d.Guarded = GuardedClient()
	}
	out := make([]tool.Tool, 0, len(names))
	wantCodeMode := false
	for _, name := range names {
		if name == vetting.RunCodeToolName {
			// Assembled AFTER this loop, over the tools it will bind — see below.
			wantCodeMode = true
			continue
		}
		ctor, ok := registry[name]
		if !ok {
			return nil, fmt.Errorf("tools: unknown builtin tool %q", name)
		}
		t, err := ctor(d)
		if err != nil {
			return nil, fmt.Errorf("tools: build %q: %w", name, err)
		}
		t, err = wrap(t, name, d)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	if wantCodeMode {
		// Code mode (run_code.go) is assembled LAST, over the tools already built
		// AND ALREADY WRAPPED above. That ordering is the whole safety argument: a
		// script's call invokes the same guarded, cancellable tool object a direct
		// call does, so the guard ladder, the cancel guard, the path jail and the
		// workspace caps all apply to it for free. Its API is generated from those
		// same tools' declarations, so it cannot drift from them.
		t, err := newRunCode(out, func(t tool.Tool) bool { return noCodeMode(t, d) })
		if err != nil {
			return nil, fmt.Errorf("tools: build %q: %w", vetting.RunCodeToolName, err)
		}
		t, err = wrap(t, vetting.RunCodeToolName, d)
		if err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// wrap applies the guard ladder (guard.go) and then the per-node cancel guard
// (cancelguard.go) to one built tool. The cancel guard is OUTERMOST, so a
// cancelled node's call is refused before it can reach the guard ladder (no
// point safety-judging or human-confirming an operation for a node the user
// just stopped).
func wrap(t tool.Tool, name string, d Deps) (tool.Tool, error) {
	var err error
	if tier, guarded := parseGuardTier(d.Guards[name]); guarded {
		t, err = newGuardedTool(t, tier, d.SafetyJudge, d.Sessions)
		if err != nil {
			return nil, fmt.Errorf("tools: guard %q: %w", name, err)
		}
	}
	if d.NodeCancelled != nil {
		t, err = newCancelGuard(t, d.NodeCancelled)
		if err != nil {
			return nil, fmt.Errorf("tools: cancel guard %q: %w", name, err)
		}
	}
	return t, nil
}

// noCodeMode reports whether a tool must stay direct-call only, i.e. must NOT
// become a function inside a script. Two kinds qualify, both for the same
// reason — a script has nowhere to suspend to:
//
//   - A CONFIRM-tier tool pauses the node for a human (guard.go). Mid-script,
//     that pause has no turn boundary to land on, and resuming would re-run the
//     script from the top — re-doing every side effect it had already performed.
//   - A LONG-RUNNING tool (ask_user, get_user_choice) ends the model's turn by
//     design and is answered on the next one.
//
// Both remain fully available as ordinary one-call-per-turn tools; code mode
// adds a path, it never removes one.
func noCodeMode(t tool.Tool, d Deps) bool {
	if t.IsLongRunning() {
		return true
	}
	tier, guarded := parseGuardTier(d.Guards[t.Name()])
	return guarded && tier.Confirm
}
