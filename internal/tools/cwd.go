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
// own root, which is where every worker starts).
//
// NODE-relative, not chat-relative, is the whole point: the node's directory is
// an INVISIBLE ROOT (a chroot, conceptually). The model never sees it, in any
// tool's arguments or results — it is applied exactly once, at the final jail
// join (jailPath), and nowhere else. There is therefore exactly ONE namespace the
// model ever speaks: paths relative to its own root (and, within it, to its cwd).
//
// The bug this design replaces: `cd` stored and reported a CHAT-relative cwd, so
// it alone handed back paths carrying the node dir ("explorer-openhands/openhands")
// while git_clone and list_dir reported the same location cwd-relative
// ("openhands"). Two incompatible namespaces; the model faithfully reused `cd`'s
// path and flailed.
//
// It lives in ctx.State() (the same persisted, resume-surviving store compaction
// uses), NOT in a mutable in-process field, so it survives compaction and
// reconnect and never leaks across sessions: the `cd` and every later tool call in
// a worker's own tool loop share that one session's state.
const CwdKey = "workspace.cwd"

// cwdFromState reads the session working directory (CwdKey) from ctx's state.
// It NEVER errors: an absent key, a nil context/state, or a non-string value
// all fall back to "" — the node's own root, which is exactly where a worker
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

// scopeFromContext derives BOTH workspace scopes a call runs under: the
// per-chat scope (the workflow/chat session id — <root>/<user>/<chatID>/…, so
// two chats never share a working tree) and the calling node's own working
// directory within it (<chatID>/<nodeID>/ — so two CONCURRENT nodes of one plan
// never share one, which is how an explorer node ended up reading another node's
// clone; see workspace.NodeDir). Both ride the SAME identity channel guard.go's
// guardSession uses — the advisor-thread marker stamped into the worker's prompt
// (the ONE channel that survives the A2A hop: a tool's own ctx.SessionID() names
// the A2A context session, not the chat) → LookupAdvisorThread → AdvisorTask
// (SessionID IS the chat id, since the DAG runs in the chat session; NodeID is
// the plan node — see internal/dag/graph.go + vetting.AdvisorTask).
//
// Returns "", "" when this call runs outside any gated node — no marker (a
// direct/un-gated invocation), or no live registration. The jail then resolves
// against the per-user root (Jail.Resolve treats "" as unscoped), exactly the
// pre-scoping behaviour: a safe fallback that never fails the call.
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
	return at.SessionID, workspace.NodeDir(at.NodeID)
}

// jailPath turns a path the MODEL wrote (node-relative, or "/"-prefixed) into the
// chat-relative path Jail.Resolve takes. It is the ONE place the node dir — the
// invisible root — is applied, which is what keeps it out of every tool argument
// and every tool result. Precedence (predictable and jail-safe):
//
//   - a path beginning with "/" is absolute WITHIN THE NODE'S OWN WORKSPACE: the cwd
//     is ignored, the node dir is still applied. "/" is the root of everything the
//     model can see, and there is nothing of the model's above it. (It used to mean
//     the CHAT root — an escape hatch into a sibling node's tree. Nothing used it,
//     it was the last way one node could reach another's clone, and it made an
//     absolute cwd un-echoable: a reported "/goose" would have resolved somewhere
//     else if fed back. One root, one namespace.)
//   - every other path is node-relative: applied to the cwd (joinCwd), then rooted
//     at the node dir.
//
// The result still flows through Jail.Resolve, which re-verifies containment (and
// resolves symlinks), so NO nodeDir + cwd + path combination can escape the jail —
// a `..` that climbs above the chat scope is caught there, escape hatch or not.
func jailPath(nodeDir, cwd, p string) string {
	if strings.HasPrefix(p, "/") {
		// "/" is the ROOT OF YOUR WORKSPACE — the node's own dir. It is not an escape
		// above it: there is nothing above it that is yours. This is what lets a tool
		// REPORT an absolute cwd ("/goose") that the model can feed straight back in;
		// when "/" meant the chat root, an absolute path was un-echoable and every
		// result had to speak in relatives ("." — which says nothing about where you
		// are). One namespace, one root, absolute or relative both legal.
		return filepath.Join(nodeDir, strings.TrimPrefix(p, "/"))
	}
	return filepath.Join(nodeDir, joinCwd(cwd, p))
}

// displayCwd renders the session working directory for a tool RESULT: the ABSOLUTE
// path of where you are standing, within the node's workspace ("/" at the root,
// "/goose" inside a clone).
//
// Every workspace tool reports it, on every call. It is absolute on purpose: a
// relative cwd of "." is technically true and operationally useless — it tells the
// model nothing about WHERE it is, so an unsure model re-derives its position by
// guessing, and each guess is a wasted turn. Live, a code-explorer `cd`'d into a
// repo, could not tell it had moved, and `cd`'d to the same place again — then
// globbed blind. Answering "where am I" costs ~20 bytes; making the model infer it
// costs turns.
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
	// Idempotent: a path that ALREADY starts with the cwd is taken as-is rather
	// than joined onto it again.
	//
	// The model is handed paths constantly — `cd` reports its new dir, `git_clone`
	// reports where it landed, `list_dir` echoes entry paths — and it very
	// reasonably feeds them straight back. Now that all of those speak ONE
	// namespace, feeding one back is unambiguous and must WORK: after `cd openhands`
	// (reporting dir "openhands"), read_file("openhands/README.md") means the same
	// file as read_file("README.md"). Doubling the cwd into
	// openhands/openhands/README.md is never what anyone means — a live explorer
	// node made 34 REPEATED calls out of 69 flailing through variants of exactly
	// that.
	if p == cwd || strings.HasPrefix(p, cwd+"/") {
		return p
	}
	return filepath.Join(cwd, p)
}

// workRoot is the absolute path of the calling node's OWN directory — the writable
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
