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
	// (Tier 0 walls — the jail, argv-only exec, etc. — still always apply).
	Guards map[string]string
	// SafetyJudge backs the guard ladder's judge tier: an independent model
	// call that allows/denies a proposed operation. nil ⇒ a tool configured
	// for a judge tier fails closed at build time (see buildGuarded).
	SafetyJudge SafetyJudge
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
	for _, name := range names {
		ctor, ok := registry[name]
		if !ok {
			return nil, fmt.Errorf("tools: unknown builtin tool %q", name)
		}
		t, err := ctor(d)
		if err != nil {
			return nil, fmt.Errorf("tools: build %q: %w", name, err)
		}
		if tier, guarded := parseGuardTier(d.Guards[name]); guarded {
			t, err = newGuardedTool(t, tier, d.SafetyJudge, d.Sessions)
			if err != nil {
				return nil, fmt.Errorf("tools: guard %q: %w", name, err)
			}
		}
		out = append(out, t)
	}
	return out, nil
}
