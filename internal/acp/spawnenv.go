package acp

import (
	"os"
	"path/filepath"

	"github.com/fagerbergj/quack/internal/workspace"
)

// SpawnEnv is the ONE place the ACP child's environment is built. Agent.spawnEnv
// delegates here and so must anything that claims to reproduce the child's env
// (quack sandbox) - a mirror drifts the first time this changes (#951).
func SpawnEnv(home string, extra []string, caps workspace.Caps) []string {
	tmp := workspace.SandboxTmpDir(caps)
	env := []string{
		"PATH=" + workspace.ChildPath(caps),
		"HOME=" + home,
		"TMPDIR=" + tmp,
		// GOTMPDIR mirrors TMPDIR: unset, Go's build work dir defaults to
		// os.TempDir(), which the jail doesn't grant (#936).
		"GOTMPDIR=" + tmp,
		"NO_COLOR=1",
		"GIT_ASKPASS=/bin/false",
		"GIT_SSH_COMMAND=/bin/false",
		"GIT_TERMINAL_PROMPT=0",
	}
	if opts := workspace.SandboxJavaToolOptions(caps); opts != "" {
		env = append(env, "JAVA_TOOL_OPTIONS="+opts)
	}
	env = append(env, extra...)
	// GOMODCACHE/GOCACHE/GOFLAGS/GOTOOLCHAIN appended LAST: exec.Cmd.Env uses
	// the last value for a duplicate key, so these win over config's
	// workspace.env default (GOMODCACHE=/usr/local/go/pkg/mod, the #940
	// preseed - READ-ONLY, outside every RW grant). Go writes cache/lock (and
	// any module the preseed lacks) even for a pure `go test`, so GOMODCACHE
	// must be a writable dir - EnsureWritableGoModCache farms one from the
	// preseed with symlinks (#954) so those offline modules still resolve.
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
