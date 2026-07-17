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
	// RunCodeGuardStandalone opts run_code out of the scriptTier floor: it is
	// guarded at ONLY its own Guards[run_code] tier, not raised to the union of
	// its bound tools' tiers. See config.Workspace.RunCodeGuardStandalone.
	RunCodeGuardStandalone bool
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
	// NodeSteerGuidance returns pending steer guidance for the DAG node a call
	// runs for, if any hasn't yet been delivered to a tool call (dag.Executor.
	// NodeSteerGuidance, wired in internal/serve). Build wraps EVERY tool in the
	// steer guard when it is set, so a steered worker's next tool call gets the
	// guidance as that call's result instead of running the real tool (see
	// steerguard.go). nil ⇒ no guard.
	NodeSteerGuidance func(chatID, nodeID string) string
}

// constructor builds one tool from the shared dependencies.
type constructor func(Deps) (tool.Tool, error)

var registry = map[string]constructor{
	"web_search":   newWebSearch,
	"web_fetch":    newFetch,
	"summarize":    newSummarize,
	"current_date": newCurrentDate,
	"stage_memory": newStageMemory,
	// Staged delivery (internal/tools/stage_delivery.go): workers stage commit/
	// push/PR/review/comment intent; the trust gate posts it exactly once, only
	// on a judge pass (internal/vetting commitDeliveryOnPass).
	"stage_pr":      newStagePR,
	"stage_review":  newStageReview,
	"stage_comment": newStageComment,
	"unstage":       newUnstage,
	"ask_user":      func(Deps) (tool.Tool, error) { return NewAskUserTool() },
	"ask_advisor":   func(d Deps) (tool.Tool, error) { return NewAskAdvisorTool(d.Advisor, d.Sessions) },
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
	// One repeat-state per build (= per agent): consecutive identical calls are
	// judged across ALL of the agent's tools within a session (see repeatguard.go).
	repeats := newRepeatStates()
	// Every tool is built ONCE and then viewed two ways:
	//
	// out       — the MODEL's direct, one-call-per-turn tools: the full guard
	// ladder (guard.go), then the cancel guard (cancelguard.go).
	// scriptAPI — the SAME tool objects as a SCRIPT sees them (run_code.go):
	// cancel-guarded, but NOT ladder-guarded. The ladder sits on
	// run_code instead, so the human and the safety judge approve the
	// whole PROGRAM once, before a line of it runs — see scriptTier.
	//
	// Neither view is unguarded, and neither is reachable without going through one
	// of these two constructions: a new tool added to the registry gets both, or it
	// gets neither.
	//
	// BOTH views are also host-path scrubbed (hostpath.go), applied here — at the one
	// wrap point — rather than in each tool: an error carrying the resolved path comes
	// from os/git, not from the tool, so every tool that wraps one with %w leaks it,
	// and a new tool would leak it too. Wrapping innermost means the script view gets
	// it as well: an in-script call's recorded error is scrubbed by the same object.
	scrub := workspaceScrub(d)
	out := make([]tool.Tool, 0, len(names))
	var scriptAPI []tool.Tool
	var floor guardTier // the strongest tier any tool the script can call carries
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
		if direct, err = steerWrap(direct, name, d); err != nil {
			return nil, err
		}
		if direct, err = cancelWrap(direct, name, d); err != nil {
			return nil, err
		}
		out = append(out, direct)

		script, err := repeatWrap(t, repeats)
		if err != nil {
			return nil, fmt.Errorf("tools: repeat guard %q: %w", name, err)
		}
		script, err = steerWrap(script, name, d)
		if err != nil {
			return nil, err
		}
		script, err = cancelWrap(script, name, d)
		if err != nil {
			return nil, err
		}
		scriptAPI = append(scriptAPI, script)
		if guarded && !noCodeMode(t) {
			floor.Judge = floor.Judge || tier.Judge
			floor.Confirm = floor.Confirm || tier.Confirm
		}
	}
	if wantCodeMode {
		// Code mode (run_code.go) is assembled LAST, over the tools built above —
		// bound to the SCRIPT view of them (jail, caps, sandbox and cancel guard
		// intact; individual guard ladder off), and itself wrapped in the guard
		// ladder at scriptTier. Its API is generated from those same tools'
		// declarations, so it cannot drift from them.
		t, err := newRunCode(scriptAPI, noCodeMode)
		if err != nil {
			return nil, fmt.Errorf("tools: build %q: %w", vetting.RunCodeToolName, err)
		}
		t = scrub(t)
		if tier, guarded := scriptTier(floor, d); guarded {
			if t, err = newGuardedTool(t, tier, d.SafetyJudge, d.Sessions); err != nil {
				return nil, fmt.Errorf("tools: guard %q: %w", vetting.RunCodeToolName, err)
			}
		}
		if t, err = steerWrap(t, vetting.RunCodeToolName, d); err != nil {
			return nil, err
		}
		if t, err = cancelWrap(t, vetting.RunCodeToolName, d); err != nil {
			return nil, err
		}
		out = append(out, t)
	}
	return out, nil
}

// scriptTier is the guard tier run_code runs under: its OWN configured tier
// (config/quack.yaml's workspace.guards — judge+confirm by default, matching
// run_command), RAISED to `floor`, the union of the tiers of every tool the script
// can call.
//
// The floor is the invariant that makes this design safe as tools are added: a
// script can do anything its tools can do, so it must be at least as guarded as the
// most-guarded tool in it. Config can only make run_code MORE guarded, never less,
// and a tool given a guard tier tomorrow cannot become reachable through a script
// under a weaker guard than its own — even if nobody remembers to touch run_code's
// entry.
//
// RunCodeGuardStandalone opts out of the floor: run_code is then guarded at only its
// own configured tier. This can lower run_code below a bound tool's guard, so it is
// for trusted single-tenant deployments where the container is the boundary and the
// in-script judge gate would only thrash the model (its whole reason to exist).
func scriptTier(floor guardTier, d Deps) (guardTier, bool) {
	tier, _ := parseGuardTier(d.Guards[vetting.RunCodeToolName])
	if !d.RunCodeGuardStandalone {
		tier.Judge = tier.Judge || floor.Judge
		tier.Confirm = tier.Confirm || floor.Confirm
	}
	return tier, tier.Judge || tier.Confirm
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

// steerWrap applies the per-node steer guard (steerguard.go), sibling to
// cancelWrap and applied on TOP of it (so a cancelled node's call is refused
// by cancelGuard before steerGuard is ever consulted — cancel wins).
func steerWrap(t tool.Tool, name string, d Deps) (tool.Tool, error) {
	if d.NodeSteerGuidance == nil {
		return t, nil
	}
	wrapped, err := newSteerGuard(t, d.NodeSteerGuidance)
	if err != nil {
		return nil, fmt.Errorf("tools: steer guard %q: %w", name, err)
	}
	return wrapped, nil
}

// noCodeMode reports whether a tool must stay direct-call only, i.e. must NOT
// become a function inside a script. Exactly one kind qualifies now: a tool that
// ENDS THE MODEL'S TURN and is answered on the NEXT one. A script has no turn
// boundary inside it to be answered on — the question would go nowhere and the
// script would sail on with a meaningless result. Two shapes of it:
//
// - ADK's long-running tools (get_user_choice), flagged IsLongRunning.
// - ask_user, which is an ordinary function tool that ends the turn by setting
// SkipSummarization; the trust GATE detects the call in the session and parks
// the node (vetting.AskToolName). An in-script call emits no session event, so
// the gate would never see the question at all.
//
// A CONFIRM-tier tool used to be excluded too, for a suspension problem of its own:
// a mid-script human pause would have had to resume by re-running the script from
// the top, re-doing every side effect it had already performed. That exclusion is
// gone, because the confirmation moved to where it belongs — run_code is ONE tool
// call, so the human approves the whole program before any of it runs, and there is
// nothing mid-script left to suspend for.
func noCodeMode(t tool.Tool) bool {
	return t.IsLongRunning() || t.Name() == vetting.AskToolName
}
