package tools

import (
	"path/filepath"
	"strings"

	"google.golang.org/adk/v2/agent"

	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// CwdKey: session-state key for agent working directory.
const CwdKey = "workspace.cwd"

// cwdFromState: reads CwdKey from session state, falls back to "".
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

// scopeFromContext: derives per-chat and per-node scopes from advisor-thread marker.
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
		wsID = at.NodeID
	}
	return at.SessionID, workspace.NodeDir(wsID)
}

// jailPath: turns a model-written path into the chat-relative path Jail.Resolve takes.
func jailPath(nodeDir, cwd, p string) string {
	p = stripSandboxRoot(p)
	if strings.HasPrefix(p, "/") {
	// "/" is the root of the node's own workspace.
		return filepath.Join(nodeDir, strings.TrimPrefix(p, "/"))
	}
	return filepath.Join(nodeDir, joinCwd(cwd, p))
}

// stripSandboxRoot: rewrites the shell's workspace-root spelling to the model's own.
func stripSandboxRoot(p string) string {
	if p == workspace.SandboxWorkRoot {
		return "/"
	}
	if rest, ok := strings.CutPrefix(p, workspace.SandboxWorkRoot+"/"); ok {
		return "/" + rest
	}
	return p
}

// displayCwd: renders the session working directory as an absolute path in the model's namespace.
func displayCwd(cwd string) string {
	if cwd == "" || cwd == "." {
		return "/"
	}
	return "/" + cwd
}

// joinCwd: applies session cwd to a node-relative path.
func joinCwd(cwd, p string) string {
	if cwd == "" || cwd == "." {
		return p
	}
	// Idempotent: path already starting with cwd is taken as-is.
	if p == cwd || strings.HasPrefix(p, cwd+"/") {
		return p
	}
	return filepath.Join(cwd, p)
}

// workRoot: absolute path of the calling node's own directory.
func (b fsBinding) workRoot() string {
	root, err := b.jail.Resolve(b.userID, b.chatID, b.nodeDir)
	if err != nil {
		return ""
	}
	return root
}
