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
	Command string `json:"command"` // a real shell command line when sandboxed; argv-split otherwise
}

type runCommandResult struct {
	ExitCode   int    `json:"exit_code"`
	Output     string `json:"output"` // combined, tail-truncated
	DurationMs int64  `json:"duration_ms"`
	// Cwd is the directory the command ran in.
	Cwd string `json:"cwd"`
}

// The tool description states what actually holds, and it differs by
// deployment, so the model is told which of the two it is living in (the
// binding knows: caps.Sandbox). An agent that believes a wall exists proposes
// operations it would otherwise not propose — and, worse, an agent that
// believes a wall exists where there is none WORKS AROUND IT: the metachar
// guard's real cost was a code-explorer writing script files to disk because it
// could not type `python3 -c "…"`.
const (
	// runCommandShellDescription — workspace.sandbox: bwrap (the default). The
	// command line goes to a real /bin/sh inside the namespace.
	runCommandShellDescription = "Run a shell command line in your workspace. `command` is executed by a real shell " +
		"(/bin/sh -c) — pipes, redirects, globs, quoting, `&&`/`;` chaining, subshells and command substitution " +
		"`$(…)` all work exactly as they do in a terminal. `dir` is the workspace-relative directory to run in " +
		"(you can also just `cd` inside the command). stdout and stderr come back merged, tail-truncated; the exit " +
		"code is the shell's. Paths: inside the shell your workspace root is " + workspace.SandboxWorkRoot + ", which " +
		"is the same place the other tools call `/` — `" + workspace.SandboxWorkRoot + "/repo` in here IS `/repo` " +
		"there, both spellings work everywhere, so a path from any tool result can be used verbatim in a command, " +
		"and a path a command prints can be handed straight back to the other tools. There is nowhere else to look. " +
		"What contains this: your command runs in an OS sandbox whose entire filesystem is a " +
		"READ-ONLY system view plus exactly two writable paths — this working directory and your isolated $HOME. " +
		"Nothing else exists in there: not ~/.ssh, not /etc/shadow, not another task's files. So there is no point " +
		"reaching outside your workspace; it will simply fail. Note this tool typically also requires independent " +
		"review and human approval before it runs (see workspace.guards) — an off-task command is denied there, and " +
		"you will wait for that review before you see any result."
	// runCommandArgvDescription — workspace.sandbox: none. With no OS boundary
	// around the child, the argv-only habit guard is all there is, so it stays.
	runCommandArgvDescription = "Run a command in your workspace. This deployment runs commands WITHOUT an OS " +
		"sandbox, so `command` is argv-split and executed directly — no shell is involved, and redirects, " +
		"backgrounding, subshells, and command/variable substitution (& ; $ < > ` ( )) are rejected. Pipes ARE " +
		"supported natively (`grep -r pattern . | head -50` chains real processes; the exit code is pipefail-style " +
		"— non-zero if ANY stage fails, with the failing stage named in the output). `dir` is the workspace-relative " +
		"directory to run in; use it instead of a `cd X &&` prefix, and write files with the write_file tool instead " +
		"of `>`. This tool typically also requires independent review and human approval before it runs (see " +
		"workspace.guards), and you will wait for that review before you see any result."
)

func newRunCommand(d Deps) (tool.Tool, error) {
	b, err := newFSBinding(d)
	if err != nil {
		return nil, err
	}
	desc := runCommandArgvDescription
	if b.shellAvailable() {
		desc = runCommandShellDescription
	}
	return functiontool.New[runCommandArgs, runCommandResult](
		functiontool.Config{Name: "run_command", Description: desc},
		func(ctx agent.Context, a runCommandArgs) (runCommandResult, error) {
			return b.withCwd(ctx).runCommand(a)
		},
	)
}

// shellAvailable reports whether run_command may hand its command line to a
// real shell: exactly when an OS sandbox contains the child. The shell is safe
// BECAUSE the namespace is real (internal/workspace/sandbox.go), so it is
// offered only when the namespace is real — with `sandbox: none` there is no
// boundary but the habit guard, and the habit guard stays.
func (b fsBinding) shellAvailable() bool {
	return b.caps.Sandbox == workspace.SandboxBwrap
}

// stripCDPrefix is UNSANDBOXED-PATH ONLY (with a shell, `cd X && …` simply
// works, and a shell cd cannot leave the sandbox's writable bind anyway).
// It recognizes the worker-LLM habit of opening a command with
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

// runCommand is run_command's logic, and it has two shapes because the
// deployment has two:
//
//   - SANDBOXED (workspace.sandbox: bwrap, the default): the command string is
//     handed to a real shell INSIDE the namespace (workspace.RunShell). No
//     metachar rejection, no argv splitting, no idiom normalization — sh does
//     all of it, correctly, and the OS namespace is the boundary (the guard
//     never was one: `sh -c "…"` contains none of the rejected characters). The
//     jail still decides the child's CWD, and the sandbox still binds only that
//     cwd + the isolated $HOME writable; a shell cannot widen that.
//   - UNSANDBOXED (workspace.sandbox: none): normalize the two harmless idioms
//     the worker LLM habitually emits (a leading `cd X &&` folds into the dir
//     resolution; a standalone `2>&1` is dropped, output is already combined),
//     split on unquoted `|`, and run argv-only through workspace.RunPipeline.
//     No metachar rejection: with no shell, metacharacters are literal argv
//     content, not operators — the guard only ever mis-fired on quoted args
//     (a grep pattern with parens) and looped the worker. The real boundary is
//     an OS sandbox (bwrap, above; #277 to enable it on the deployment).
//
// Either way `dir` resolves through the jail exactly as it always did, and the
// runner is workspace's — one internal runner, two consumers (run_command and
// the trust gate's deterministic checks; see .quack/plan-pr5-tool-schemas.md
// §4/§4b). The gate's `checks` never take this shell path at all: they are
// prefix-matched against operator config, and a prefix allowlist means nothing
// if the suffix can open a shell.
func (b fsBinding) runCommand(a runCommandArgs) (runCommandResult, error) {
	if strings.TrimSpace(a.Command) == "" {
		return runCommandResult{}, fmt.Errorf("run_command: command must not be empty")
	}
	command, dirArg := a.Command, a.Dir
	var stages [][]string
	shell := b.shellAvailable()
	if !shell {
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
		// No metachar rejection: with no shell, metacharacters are never
		// operators — SplitArgv/SplitPipeline treat them as literal argv content
		// (a quoted grep pattern like 'func (e *Extension)' is one arg). The
		// rejection only ever mis-fired on legitimate quoted args and looped the
		// worker; the real boundary is an OS sandbox (see #277), not this guard.
		var err error
		if stages, err = workspace.SplitPipeline(command); err != nil {
			return runCommandResult{}, fmt.Errorf("run_command: %w", err)
		}
	}
	// Apply the session cwd ONCE to the final dir argument (after the #154
	// `cd X &&` fold above already produced dirArg). The in-command `cd` and
	// the session cwd never double-apply the SAME cwd: the fold shapes dirArg,
	// then b.resolve joins the session cwd onto it a single time. (On the shell
	// path there is no fold: a `cd` inside the command is the shell's own, and
	// it can only move the child WITHIN the sandbox's writable bind.)
	dir, err := b.resolve(dirArg)
	if err != nil {
		return runCommandResult{}, err
	}
	if info, statErr := os.Stat(dir); statErr != nil || !info.IsDir() {
		return runCommandResult{}, fmt.Errorf("run_command: %q is not a directory", dirArg)
	}

	// The sandbox's writable subtree is the NODE's own dir, not just this command's
	// cwd — see workspace.Caps.WorkRoot. Binding the cwd alone silently ate every
	// file a child wrote elsewhere in its own workspace (a `git clone` one level up
	// vanished), because a private /tmp hides a workspace root that lives under /tmp.
	caps := b.caps
	caps.WorkRoot = b.workRoot()

	t0 := time.Now()
	var res workspace.ExecResult
	if shell {
		res, err = workspace.RunShell(context.Background(), dir, command, caps)
	} else {
		res, err = workspace.RunPipeline(context.Background(), dir, stages, caps)
	}
	dur := time.Since(t0).Milliseconds()
	if err != nil {
		return runCommandResult{}, fmt.Errorf("run_command: %w", err)
	}
	slog.Info("workspace exec", "component", "tools", "tool", "run_command",
		"user", b.userID, "dir", dirArg, "command", command, "shell", shell, "exit", res.ExitCode, "duration_ms", dur)
	return runCommandResult{ExitCode: res.ExitCode, Output: res.Output, DurationMs: dur, Cwd: displayCwd(b.cwd)}, nil
}
