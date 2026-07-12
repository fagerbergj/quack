package tools

import (
	"path/filepath"
	"strings"

	"google.golang.org/adk/v2/agent"
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
