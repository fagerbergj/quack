package tools

import (
	"context"
	"errors"
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

// token is the calling node's advisor token: fixed at build, else set on ctx by an in-process
// caller (the judge round). Never the prompt's last marker, which gate-appended text can forge.
func (s CallScope) token(ctx context.Context) string {
	if s.AdvisorToken != "" || ctx == nil {
		return s.AdvisorToken
	}
	return vetting.AdvisorTokenFromContext(ctx)
}

// advisorTask is the calling node's registered AdvisorTask; false outside a node or on a miss.
func (s CallScope) advisorTask(ctx context.Context) (vetting.AdvisorTask, bool) {
	if tok := s.token(ctx); tok != "" {
		return vetting.LookupAdvisorThread(tok)
	}
	return vetting.AdvisorTask{}, false
}

// errNodeScopeGone fails fs calls closed: the unscoped fallback is the whole user root.
var errNodeScopeGone = errors.New("workspace: this node's scope is not registered; refusing to resolve paths outside it")

// fsScope is the chat and node dir fs tools resolve under; outside a node it is unscoped.
func (s CallScope) fsScope(ctx context.Context) (chatID, nodeDir string, err error) {
	if s.token(ctx) == "" {
		return "", "", nil
	}
	at, ok := s.advisorTask(ctx)
	if !ok {
		return "", "", errNodeScopeGone
	}
	wsID := at.WorkspaceNodeID
	if wsID == "" {
		wsID = at.NodeID
	}
	return at.ChatID, workspace.NodeDir(wsID), nil
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
	if b.scopeErr != nil {
		return ""
	}
	root, err := b.jail.Resolve(b.userID, b.chatID, b.nodeDir)
	if err != nil {
		return ""
	}
	return root
}
