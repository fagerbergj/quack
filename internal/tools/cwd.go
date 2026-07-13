package tools

import (
	"path/filepath"
	"strings"

	"google.golang.org/adk/v2/agent"

	"github.com/fagerbergj/quack/internal/vetting"
)

// CwdKey is the session-state key the `cd` tool stores the agent's working
// directory under: a jail-relative, slash-separated path ("" = jail root).
// Every workspace tool that takes a path/dir reads it (cwdFromState) and
// resolves a RELATIVE argument against it — the durable equivalent of a shell's
// cwd. It lives in ctx.State() (the same persisted, resume-surviving store
// compaction uses), NOT in a mutable in-process field, so it survives
// compaction and reconnect and never leaks across sessions: the `cd` and every
// later tool call in a worker's own tool loop share that one session's state.
const CwdKey = "workspace.cwd"

// cwdFromState reads the session working directory (CwdKey) from ctx's state.
// It NEVER errors: an absent key, a nil context/state, or a non-string value
// all fall back to "" (the jail root — exactly today's behaviour), so any tool
// call made WITHOUT a prior `cd` behaves as it always did.
func cwdFromState(ctx agent.Context) string {
	if ctx == nil {
		return ""
	}
	st := ctx.State()
	if st == nil {
		return ""
	}
	v, err := st.Get(CwdKey)
	if err != nil {
		return ""
	}
	s, _ := v.(string)
	return s
}

// chatScopeFromContext derives the per-chat workspace scope — the workflow/chat
// session id — that every workspace tool resolves paths under, so two chats
// never share a working tree (<root>/<user>/<chatID>/…). It reuses the SAME
// identity channel guard.go's guardSession does: the advisor-thread marker
// stamped into the worker's prompt (the ONE channel that survives the A2A hop —
// a tool's own ctx.SessionID() names the A2A context session, not the chat) →
// LookupAdvisorThread → AdvisorTask.SessionID (which IS the chat id, since the
// DAG runs in the chat session; see internal/dag/graph.go + vetting.AdvisorTask).
//
// Returns "" when this call runs outside any gated node — no marker (a direct/
// un-gated invocation), or no live registration. The jail then resolves against
// the per-user root (Jail.Resolve treats "" as unscoped), exactly today's
// behaviour: a safe fallback that never fails the call, though it forgoes the
// per-chat isolation for that one call.
func chatScopeFromContext(ctx agent.Context) string {
	if ctx == nil {
		return ""
	}
	token, ok := vetting.ParseAdvisorThread(contentText(ctx.UserContent()))
	if !ok {
		return ""
	}
	at, ok := vetting.LookupAdvisorThread(token)
	if !ok {
		return ""
	}
	return at.SessionID
}

// joinCwd applies the session working directory to a workspace path, yielding
// a jail-relative path for Jail.Resolve. Precedence (predictable and jail-safe):
//
//   - a path beginning with "/" is JAIL-ROOT-relative — the explicit escape
//     hatch back to the workspace root, with cwd IGNORED (Jail.Resolve rejected
//     a leading "/" as an absolute path before, so no previously-valid input
//     regresses; the leading "/" is stripped, never passed to Resolve).
//   - every other path is relative to cwd ("" cwd = jail root = today).
//
// The result still flows through Jail.Resolve, which re-verifies containment
// (and resolves symlinks), so NO cwd + path combination can escape the jail —
// a `..` that climbs above cwd is caught there, exactly as a bare `..` is today.
func joinCwd(cwd, p string) string {
	if strings.HasPrefix(p, "/") {
		return strings.TrimPrefix(p, "/")
	}
	if cwd == "" {
		return p
	}
	return filepath.Join(cwd, p)
}
