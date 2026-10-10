package acp

import (
	"os"
	"path/filepath"

	"github.com/fagerbergj/quack/internal/workspace"
)

// SpawnEnv is the one place the ACP child's environment is built; anything reproducing the child's env
// (quack sandbox) must call it, since a mirror drifts.
func SpawnEnv(home string, extra []string, caps workspace.Caps) []string {
	tmp := workspace.SandboxTmpDir(caps)
	env := []string{
		"PATH=" + workspace.ChildPath(caps),
		"HOME=" + home,
		"TMPDIR=" + tmp,
		// GOTMPDIR mirrors TMPDIR: unset, Go's build work dir defaults to os.TempDir(), which the jail doesn't grant.
		"GOTMPDIR=" + tmp,
		"NO_COLOR=1",
		"GIT_ASKPASS=/bin/false",
		"GIT_SSH_COMMAND=/bin/false",
		"GIT_TERMINAL_PROMPT=0",
	}
	if opts := workspace.SandboxJavaToolOptions(caps); opts != "" {
		env = append(env, "JAVA_TOOL_OPTIONS="+opts)
	}
	if caps.ACPStateDir != "" {
		env = append(env, "PI_ACP_STATE_DIR="+caps.ACPStateDir)
	}
	env = append(env, extra...)
	// Go vars go last so they win over workspace.env's read-only GOMODCACHE preseed (Cmd.Env keeps the last
	// duplicate). Go writes cache/locks even for go test; EnsureWritableGoModCache symlinks the preseed.
	goCache := filepath.Join(home, ".cache", "go-build")
	_ = os.MkdirAll(goCache, 0o755)
	env = append(env,
		"GOMODCACHE="+workspace.EnsureWritableGoModCache(home),
		"GOCACHE="+goCache,
		"GOFLAGS=-mod=mod",
		"GOTOOLCHAIN=local",
	)
	return env
}
