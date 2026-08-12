package workspace

import (
	"context"
	"log/slog"
	"sync"
)

// setupCache: has check_setup already run in this dir? One dir gets one
// bootstrap - repeat gate rounds, worker rounds, and re-provisioning of the
// same dir must not re-run it. Shared across every RunCheckSetup caller
// (clone/worktree provisioning and the gate's own checks), keyed by the
// jail-resolved absolute path, so a dir bootstrapped at provisioning time is
// a cache hit (no-op) when the gate later asks for the same dir.
var setupCache sync.Map // key: dir → struct{}

// RunCheckSetup runs setup once per dir, in order, before dir is used for
// anything else - checks (vetting), a worker's own tree, or a read-only
// worktree linked off the shared clone (workers can't bootstrap a read-only
// tree by definition). A failing step logs Warn and stops: a broken
// bootstrap must not become a new way to fail a node or a provisioning call.
func RunCheckSetup(dir string, setup []string, caps Caps) {
	if len(setup) == 0 {
		return
	}
	if _, already := setupCache.LoadOrStore(dir, struct{}{}); already {
		return
	}
	for _, cmd := range setup {
		stages, err := SplitPipeline(cmd)
		if err != nil {
			slog.Warn("check_setup command invalid; skipping remaining setup", "component", "workspace", "dir", dir, "cmd", cmd, "err", err)
			return
		}
		res, err := RunPipeline(context.Background(), dir, stages, caps)
		if err != nil {
			slog.Warn("check_setup command failed to run; proceeding without it", "component", "workspace", "dir", dir, "cmd", cmd, "err", err)
			return
		}
		if res.ExitCode != 0 {
			slog.Warn("check_setup command exited non-zero; proceeding without it", "component", "workspace", "dir", dir, "cmd", cmd, "exit_code", res.ExitCode, "output", res.Output)
			return
		}
	}
}
