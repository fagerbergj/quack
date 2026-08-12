package vetting

import (
	"context"
	"log/slog"
	"sync"

	"github.com/fagerbergj/quack/internal/workspace"
)

// setupCache: has check_setup already run in this dir? One provisioned clone
// gets one bootstrap - repeat gate rounds on the same node must not re-run it.
var setupCache sync.Map // key: dir → struct{}

// runCheckSetup runs cfg.CheckSetup once per dir, in order, before checks are
// derived or run - callers use it both for the worker's own tree (checks.go)
// and the ephemeral base worktree (baseline.go), since the latter is a fresh
// checkout that never saw the former's setup. A failing step logs Warn and
// stops: the base-failure self-disarm already protects the worker from repo
// debt, and a broken bootstrap must not become a new way to fail a node.
func runCheckSetup(dir string, setup []string, caps workspace.Caps) {
	if len(setup) == 0 {
		return
	}
	if _, already := setupCache.LoadOrStore(dir, struct{}{}); already {
		return
	}
	for _, cmd := range setup {
		stages, err := workspace.SplitPipeline(cmd)
		if err != nil {
			slog.Warn("check_setup command invalid; skipping remaining setup", "component", "vetting", "dir", dir, "cmd", cmd, "err", err)
			return
		}
		res, err := workspace.RunPipeline(context.Background(), dir, stages, caps)
		if err != nil {
			slog.Warn("check_setup command failed to run; checks proceed without it", "component", "vetting", "dir", dir, "cmd", cmd, "err", err)
			return
		}
		if res.ExitCode != 0 {
			slog.Warn("check_setup command exited non-zero; checks proceed without it", "component", "vetting", "dir", dir, "cmd", cmd, "exit_code", res.ExitCode, "output", res.Output)
			return
		}
	}
}
