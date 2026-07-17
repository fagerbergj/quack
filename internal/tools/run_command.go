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
	Command string `json:"command"` // a real shell command line — see runCommandDescription
}

type runCommandResult struct {
	ExitCode   int    `json:"exit_code"`
	Output     string `json:"output"` // combined, tail-truncated
	DurationMs int64  `json:"duration_ms"`
	// Cwd is the directory the command ran in.
	Cwd string `json:"cwd"`
}

// runCommandDescription is built once, at tool-construction time, from the
// deployment's OWN caps.Sandbox — the sandbox status is a deployment-wide
// constant, not a per-call decision, so the model is told plainly which of the
// two it is living in. An agent that believes a wall exists where there is
// none WORKS AROUND IT: the old argv-only guard's real cost was a
// code-explorer writing script files to disk because it could not type
// `python3 -c "…"` (#277).
func runCommandDescription(caps workspace.Caps) string {
	desc := "Run a shell command line in your workspace. `command` is executed by a real shell " +
		"(/bin/sh -c) — pipes, redirects, globs, quoting, `&&`/`;` chaining, subshells and command " +
		"substitution `$(…)` all work exactly as they do in a terminal. `dir` is the workspace-relative " +
		"directory to run in (you can also just `cd` inside the command). stdout and stderr come back " +
		"merged, tail-truncated; the exit code is the shell's. This tool typically also requires " +
		"independent review and human approval before it runs (see workspace.guards) — an off-task " +
		"command is denied there, and you will wait for that review before you see any result."
	if caps.Sandbox == workspace.SandboxBwrap {
		return desc + " Paths: inside the shell your workspace root is " + workspace.SandboxWorkRoot + ", which " +
			"is the same place the other tools call `/` — `" + workspace.SandboxWorkRoot + "/repo` in here IS `/repo` " +
			"there, both spellings work everywhere, so a path from any tool result can be used verbatim in a command, " +
			"and a path a command prints can be handed straight back to the other tools. There is nowhere else to look. " +
			"What contains this: your command runs in an OS sandbox whose entire filesystem is a " +
			"READ-ONLY system view plus exactly two writable paths — this working directory and your isolated $HOME. " +
			"Nothing else exists in there: not ~/.ssh, not /etc/shadow, not another task's files. So there is no point " +
			"reaching outside your workspace; it will simply fail."
	}
	return desc + " This deployment runs WITHOUT an OS sandbox: your command has the server user's FULL " +
		"filesystem authority — whatever that account can read or write (~/.ssh, ~/.aws, ~/.config/gh, .env, " +
		"anything else it can reach), your command can too. `dir` still confines where a bare relative path " +
		"lands, but nothing here stops a command that names an absolute path outside it. Treat every command " +
		"as if it runs directly on the host, because it does."
}

func newRunCommand(d Deps) (tool.Tool, error) {
	b, err := newFSBinding(d)
	if err != nil {
		return nil, err
	}
	return functiontool.New[runCommandArgs, runCommandResult](
		functiontool.Config{Name: "run_command", Description: runCommandDescription(b.caps)},
		func(ctx agent.Context, a runCommandArgs) (runCommandResult, error) {
			return b.withCwd(ctx).runCommand(a)
		},
	)
}

// runCommand always hands its command line to a real shell (workspace.RunShell)
// — ONE runner, whatever the deployment's sandbox mode: childArgv wraps the
// child in bwrap when caps.Sandbox == SandboxBwrap, and runs a bare
// `/bin/sh -c` otherwise (internal/workspace/sandbox.go). There is no
// argv-split fallback: with a real shell, metachars/globs/redirects/`$(…)`
// are never mis-handled, sandboxed or not — the OS namespace is the boundary
// where one exists, and the server user's own authority is the boundary
// where it doesn't (#277).
//
// The gate's `checks` never take this path at all: they are prefix-matched
// against operator config (internal/vetting/checks.go), and a prefix
// allowlist means nothing if the suffix can open a shell.
func (b fsBinding) runCommand(a runCommandArgs) (runCommandResult, error) {
	if strings.TrimSpace(a.Command) == "" {
		return runCommandResult{}, fmt.Errorf("run_command: command must not be empty")
	}
	dir, err := b.resolve(a.Dir)
	if err != nil {
		return runCommandResult{}, err
	}
	if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
		return runCommandResult{}, fmt.Errorf("run_command: %q is not a directory", a.Dir)
	}

	// The sandbox's writable subtree is the NODE's own dir, not just this command's
	// cwd — see workspace.Caps.WorkRoot. Binding the cwd alone silently ate every
	// file a child wrote elsewhere in its own workspace (a `git clone` one level up
	// vanished), because a private /tmp hides a workspace root that lives under /tmp.
	caps := b.caps
	caps.WorkRoot = b.workRoot()

	t0 := time.Now()
	res, err := workspace.RunShell(context.Background(), dir, a.Command, caps)
	dur := time.Since(t0).Milliseconds()
	if err != nil {
		return runCommandResult{}, fmt.Errorf("run_command: %w", err)
	}
	slog.Info("workspace exec", "component", "tools", "tool", "run_command",
		"user", b.userID, "dir", a.Dir, "command", a.Command, "sandbox", caps.Sandbox, "exit", res.ExitCode, "duration_ms", dur)
	return runCommandResult{ExitCode: res.ExitCode, Output: res.Output, DurationMs: dur, Cwd: displayCwd(b.cwd)}, nil
}
