package tools

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"strings"
	"time"

	"google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/tool"
	"google.golang.org/adk/v2/tool/functiontool"

	"github.com/fagerbergj/quack/internal/workspace"
)

// ---------------------------------------------------------------------------
// run_command (see .quack/plan-pr5-tool-schemas.md §4b)
// ---------------------------------------------------------------------------

type runCommandArgs struct {
	Dir     string `json:"dir"`
	Command string `json:"command"` // argv-split, no shell interpretation
}

type runCommandResult struct {
	ExitCode   int    `json:"exit_code"`
	Output     string `json:"output"` // combined, tail-truncated
	DurationMs int64  `json:"duration_ms"`
}

// runCommandDescription documents the Tier-0 walls (see internal/workspace/
// exec.go's RunArgv/RunPipeline) and the honest limit the design doc calls
// out: the jail confines the cwd, not what an arbitrary binary DOES. That's
// exactly why run_command ships judge+confirm-guarded by default
// (workspace.guards) — the operator, who knows what's on their host, owns the
// dial.
const runCommandDescription = "Run a command in your workspace. `command` is argv-split — NEVER a shell. Pipes ARE " +
	"supported natively (`grep -r pattern . | head -50` chains real processes; exit code is pipefail-style — " +
	"non-zero if ANY stage fails, with the failing stage named in the output). Everything else a shell would " +
	"interpret — redirects, backgrounding, subshells, command/variable substitution (& ; $ < > ` ( )) — is " +
	"rejected outright and unavailable. `dir` is the workspace-relative directory to run in. Honest limit: the " +
	"jail confines the working directory, not what the program itself does — this tool typically requires " +
	"independent review and human approval before it runs (see workspace.guards), so expect to wait for that " +
	"before you see a result."

func newRunCommand(d Deps) (tool.Tool, error) {
	b, err := newFSBinding(d)
	if err != nil {
		return nil, err
	}
	return functiontool.New[runCommandArgs, runCommandResult](
		functiontool.Config{Name: "run_command", Description: runCommandDescription},
		func(_ agent.Context, a runCommandArgs) (runCommandResult, error) { return b.runCommand(a) },
	)
}

// runCommand is run_command's logic: validate (no shell metacharacters;
// pipeline-split on unquoted `|`), resolve `dir` through the jail, then
// execute via the SAME runner the trust gate's deterministic checks use
// (workspace.RunPipeline) — one internal runner, two consumers (see
// .quack/plan-pr5-tool-schemas.md §4/§4b).
func (b fsBinding) runCommand(a runCommandArgs) (runCommandResult, error) {
	if strings.TrimSpace(a.Command) == "" {
		return runCommandResult{}, fmt.Errorf("run_command: command must not be empty")
	}
	if workspace.ContainsShellMetachar(a.Command) {
		return runCommandResult{}, fmt.Errorf(
			"run_command: command contains a shell metacharacter (& ; $ < > ` ( )) — run_command never invokes a " +
				"shell; pipes are supported natively, but redirects, backgrounding, subshells, and substitution are " +
				"unavailable")
	}
	stages, err := workspace.SplitPipeline(a.Command)
	if err != nil {
		return runCommandResult{}, fmt.Errorf("run_command: %w", err)
	}
	dir, err := b.jail.Resolve(b.userID, a.Dir)
	if err != nil {
		return runCommandResult{}, err
	}
	if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
		return runCommandResult{}, fmt.Errorf("run_command: %q is not a directory", a.Dir)
	}

	t0 := time.Now()
	res, err := workspace.RunPipeline(context.Background(), dir, stages, b.caps)
	dur := time.Since(t0).Milliseconds()
	if err != nil {
		return runCommandResult{}, fmt.Errorf("run_command: %w", err)
	}
	slog.Info("workspace exec", "component", "tools", "tool", "run_command",
		"user", b.userID, "dir", a.Dir, "command", a.Command, "stages", len(stages), "exit", res.ExitCode, "duration_ms", dur)
	return runCommandResult{ExitCode: res.ExitCode, Output: res.Output, DurationMs: dur}, nil
}
