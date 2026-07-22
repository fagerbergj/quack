package tools

import (
	"path/filepath"
	"strings"

	"google.golang.org/adk/v2/agent"

	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// CwdKey is the session-state key the `cd` tool stores the agent's working
// directory under: a NODE-relative, slash-separated path ("" or "." = the node's
// own root, where every worker starts).
//
// NODE-relative, not chat-relative: the node's directory is an INVISIBLE ROOT,
// applied exactly once at the final jail join (jailPath), so the model speaks
// exactly ONE namespace in every tool's arguments and results. (A chat-relative
// cwd once leaked the node dir and split the namespace in two.)
//
// It lives in ctx.State(), not an in-process field, so it survives compaction and
// reconnect and never leaks across sessions.
const CwdKey = "workspace.cwd"

// cwdFromState reads the session working directory (CwdKey) from ctx's state.
// It NEVER errors: an absent key, a nil context/state, or a non-string value
// all fall back to "" - the node's own root, which is exactly where a worker
// that has not cd'd anywhere should be.
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

// scopeFromContext derives both workspace scopes a call runs under: the per-chat
// scope (two chats never share a working tree) and the calling node's directory
// within it (two CONCURRENT nodes of one plan never share one). Both ride the
// advisor-thread marker stamped into the worker's prompt - the one identity
// channel that survives the A2A hop (a tool's ctx.SessionID() names the A2A
// context session, not the chat).
//
// Returns "", "" outside any gated node; the jail then resolves against the
// per-user root - a safe fallback that never fails the call.
func scopeFromContext(ctx agent.Context) (chatID, nodeDir string) {
	if ctx == nil {
		return "", ""
	}
	token, ok := vetting.ParseAdvisorThread(contentText(ctx.UserContent()))
	if !ok {
		return "", ""
	}
	at, ok := vetting.LookupAdvisorThread(token)
	if !ok {
		return "", ""
	}
	wsID := at.WorkspaceNodeID
	if wsID == "" {
		wsID = at.NodeID // default: a node's own dir (unset outside a shared-clone chain)
	}
	return at.SessionID, workspace.NodeDir(wsID)
}

// jailPath turns a path the MODEL wrote (node-relative, or "/"-prefixed) into the
// chat-relative path Jail.Resolve takes. It is the ONE place the node dir - the
// invisible root - is applied, which is what keeps it out of every tool argument
// and every tool result. Precedence (predictable and jail-safe):
//
//   - a path beginning with "/" is absolute WITHIN THE NODE'S OWN WORKSPACE: the cwd
//     is ignored, the node dir is still applied. (It used to mean the CHAT root -
//     the last way one node could reach a sibling's clone, and it made a reported
//     absolute cwd un-echoable. One root, one namespace.)
//   - every other path is node-relative: applied to the cwd (joinCwd), then rooted
//     at the node dir.
//
// The result still flows through Jail.Resolve, which re-verifies containment (and
// resolves symlinks), so NO nodeDir + cwd + path combination can escape the jail -
// a `..` that climbs above the chat scope is caught there, escape hatch or not.
func jailPath(nodeDir, cwd, p string) string {
	p = stripSandboxRoot(p)
	if strings.HasPrefix(p, "/") {
		// "/" is the root of the node's own workspace, so a reported absolute cwd
		// can be fed straight back in.
		return filepath.Join(nodeDir, strings.TrimPrefix(p, "/"))
	}
	return filepath.Join(nodeDir, joinCwd(cwd, p))
}

// stripSandboxRoot turns the SHELL's spelling of the workspace root into the
// model's own: "/workspace/quack/main.go" → "/quack/main.go", so a path out of a
// shell result (`pwd`) feeds back into any fs tool. (The sandbox closes the other
// direction - see rootAliasArgs.) Only the ABSOLUTE alias is rewritten, so a
// relative "workspace/x" still means an entry actually named workspace; a
// top-level entry named "workspace" is shadowed in the absolute spelling only.
func stripSandboxRoot(p string) string {
	if p == workspace.SandboxWorkRoot {
		return "/"
	}
	if rest, ok := strings.CutPrefix(p, workspace.SandboxWorkRoot+"/"); ok {
		return "/" + rest
	}
	return p
}

// displayCwd renders the session working directory for a tool RESULT: the
// ABSOLUTE path of where you are standing ("/" at the root, "/goose" inside a
// clone). Absolute on purpose - a relative "." tells the model nothing about
// where it is, and it wastes turns re-deriving its position.
func displayCwd(cwd string) string {
	if cwd == "" || cwd == "." {
		return "/"
	}
	return "/" + cwd
}

// joinCwd applies the session working directory to a node-relative path, yielding
// another node-relative path. Both sides speak the ONE namespace the model sees;
// the node dir is added afterwards, by jailPath.
func joinCwd(cwd, p string) string {
	if cwd == "" || cwd == "." {
		return p
	}
	// Idempotent: a path that ALREADY starts with the cwd is taken as-is, never
	// joined onto it again - tools echo paths and the model feeds them back, and
	// doubling the cwd is never what anyone means.
	if p == cwd || strings.HasPrefix(p, cwd+"/") {
		return p
	}
	return filepath.Join(cwd, p)
}

// workRoot is the absolute path of the calling node's OWN directory - the writable
// subtree a sandboxed child gets (workspace.Caps.WorkRoot). It is the invisible root
// every model-supplied path already resolves under (jailPath), so a child that can
// write anywhere the fs tools can write is not a widening: it is the SAME workspace.
// "" when there is no node scope (an un-gated call), leaving the cwd bound alone.
func (b fsBinding) workRoot() string {
	root, err := b.jail.Resolve(b.userID, b.chatID, b.nodeDir)
	if err != nil {
		return ""
	}
	return root
}
