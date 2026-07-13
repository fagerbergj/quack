package vetting

import (
	"context"
	"fmt"

	"github.com/fagerbergj/quack/internal/workspace"
)

// checksPassCriterion is the GATE side of §4 (orchestrator-set deterministic
// gates): it runs cfg.Checks — already plan-time validated argv-safe commands
// (see dag/planner.go's validateChecks) — via the SAME jailed runner
// run_command uses (workspace.RunPipeline; pipes are native, everything else
// a shell would interpret stays unexpressible), stopping at the first
// failure. Called from foldDeterministic (node.go) exactly like
// grounded_in_retrieval: a failing check folds in as criterion `checks_pass`
// with Score 0 (weakest-link — one failing check sinks the round on its own)
// and a Reason naming the command plus its output tail, so composeFeedback
// (node.go) carries the actual compiler/test failure into the revise prompt.
// All checks passing scores 1. Only called when len(cfg.Checks) > 0 — a node
// without checks is entirely untouched by this criterion.
func checksPassCriterion(cfg Config) criterionScore {
	if cfg.Workspace == nil {
		// A node with Checks set but no workspace wired up is a config/wiring
		// bug (internal/serve's buildAgents didn't stamp Workspace onto the
		// base Config), not a model or user error — fail closed rather than
		// running unjailed.
		return criterionScore{Score: 0, Reason: "deterministic: this node has checks configured but no workspace is wired up (internal error — contact the operator)"}
	}
	dir, err := cfg.Workspace.Resolve(cfg.WorkspaceUserID, cfg.ChatID, cfg.Workdir)
	if err != nil {
		return criterionScore{Score: 0, Reason: fmt.Sprintf("deterministic: checks workdir %q: %v", cfg.Workdir, err)}
	}
	for _, check := range cfg.Checks {
		stages, err := workspace.SplitPipeline(check)
		if err != nil {
			return criterionScore{Score: 0, Reason: fmt.Sprintf("deterministic: check %q: %v", check, err)}
		}
		res, err := workspace.RunPipeline(context.Background(), dir, stages, cfg.WorkspaceCaps)
		if err != nil {
			return criterionScore{Score: 0, Reason: fmt.Sprintf("deterministic: check %q: %v", check, err)}
		}
		if res.ExitCode != 0 {
			return criterionScore{Score: 0, Reason: fmt.Sprintf(
				"deterministic: check %q failed (exit %d):\n%s", check, res.ExitCode, res.Output)}
		}
	}
	return criterionScore{Score: 1, Reason: fmt.Sprintf("deterministic: %d check(s) passed", len(cfg.Checks))}
}
