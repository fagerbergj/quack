package workspace

import (
	"context"
	"log/slog"
	"sync"
)

// setupCache: one bootstrap per jail-resolved dir, shared by provisioning and the gate's checks.
var setupCache sync.Map // key: dir → struct{}

// RunCheckSetup: a failing step warns and stops; a broken bootstrap must not fail a node or provisioning.
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
