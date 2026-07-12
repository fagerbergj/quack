package tools

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"path/filepath"
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

// stripCDPrefix recognizes the worker-LLM habit of opening a command with
// `cd <path> && <rest>` — a shell re-statement of the `dir` argument that
// would otherwise trip the metachar wall on `&&` and burn a revise round
// (observed live: `cd repo && npx vitest run …` with dir already "repo").
// Only a single LEADING `cd` with a bare, unquoted, metachar-free path is
// stripped; a quoted path (`cd "my dir" &&`) is left alone — the metachar
// wall's coaching error still fires for that rarity, which is acceptable.
// Everything after the first `&&` is returned verbatim, so further chaining
// (`cd x && y && z`) still hits the wall: only the cd prefix is special.
func stripCDPrefix(command string) (target, rest string, ok bool) {
	s := strings.TrimSpace(command)
	if !strings.HasPrefix(s, "cd") {
		return "", "", false
	}
	s = s[2:]
	if s == "" || (s[0] != ' ' && s[0] != '\t') {
		return "", "", false // "cdrecord …" etc., not a cd
	}
	s = strings.TrimLeft(s, " \t")
	i := strings.IndexAny(s, " \t")
	if i < 0 {
		return "", "", false // bare `cd x` — no `&&`, not the idiom
	}
	target = s[:i]
	rest = strings.TrimSpace(s[i:])
	if !strings.HasPrefix(rest, "&&") {
		return "", "", false // `cd x; y` and friends — the wall handles them
	}
	rest = strings.TrimSpace(rest[2:])
	if target == "" || rest == "" ||
		strings.HasPrefix(target, "'") || strings.HasPrefix(target, `"`) ||
		strings.ContainsRune(target, '|') || workspace.ContainsShellMetachar(target) {
		return "", "", false
	}
	return target, rest, true
}

// runCommand is run_command's logic: normalize two rejected-but-harmless
// shell idioms the worker LLM habitually emits (a leading `cd X &&` folds
// into the dir resolution; a standalone `2>&1` is dropped — output is already
// combined), then validate (no shell metacharacters; pipeline-split on
// unquoted `|`), resolve `dir` through the jail, and execute via the SAME
// runner the trust gate's deterministic checks use (workspace.RunPipeline) —
// one internal runner, two consumers (see .quack/plan-pr5-tool-schemas.md
// §4/§4b). The normalization is run_command-only: the gate's checks come from
// operator config, not an LLM, and keep their exact semantics.
func (b fsBinding) runCommand(a runCommandArgs) (runCommandResult, error) {
	if strings.TrimSpace(a.Command) == "" {
		return runCommandResult{}, fmt.Errorf("run_command: command must not be empty")
	}
	command, dirArg := a.Command, a.Dir
	if target, rest, ok := stripCDPrefix(command); ok {
		command = rest
		switch {
		case strings.TrimSpace(dirArg) == "":
			dirArg = target // `cd` supplies the missing dir argument
		case filepath.IsAbs(target):
			dirArg = target // let jail.Resolve reject it exactly as a dir argument would be
		case filepath.Clean(dirArg) == filepath.Clean(target):
			// dir already names the cd target (the habit re-states it) —
			// don't double repo to repo/repo.
		default:
			// Shell semantics: the cd runs from dir, so a relative target
			// composes under it. `..` components survive the Join and are
			// judged by the jail like any other dir argument.
			dirArg = filepath.Join(dirArg, target)
		}
	}
	command = workspace.StripStderrMerge(command)
	if strings.TrimSpace(command) == "" {
		return runCommandResult{}, fmt.Errorf("run_command: command must not be empty")
	}
	if workspace.ContainsShellMetachar(command) {
		return runCommandResult{}, fmt.Errorf(
			"run_command: command contains a shell metacharacter (& ; $ < > ` ( )) — run_command never invokes a " +
				"shell; pipes are supported natively, but redirects, backgrounding, subshells, and substitution are " +
				"unavailable. Use the `dir` argument instead of a 'cd X &&' prefix; stderr is already merged into " +
				"the output (drop '2>&1'); write output to a file with the write_file tool instead of '>'")
	}
	stages, err := workspace.SplitPipeline(command)
	if err != nil {
		return runCommandResult{}, fmt.Errorf("run_command: %w", err)
	}
	dir, err := b.jail.Resolve(b.userID, dirArg)
	if err != nil {
		return runCommandResult{}, err
	}
	if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
		return runCommandResult{}, fmt.Errorf("run_command: %q is not a directory", dirArg)
	}

	t0 := time.Now()
	res, err := workspace.RunPipeline(context.Background(), dir, stages, b.caps)
	dur := time.Since(t0).Milliseconds()
	if err != nil {
		return runCommandResult{}, fmt.Errorf("run_command: %w", err)
	}
	slog.Info("workspace exec", "component", "tools", "tool", "run_command",
		"user", b.userID, "dir", dirArg, "command", command, "stages", len(stages), "exit", res.ExitCode, "duration_ms", dur)
	return runCommandResult{ExitCode: res.ExitCode, Output: res.Output, DurationMs: dur}, nil
}
