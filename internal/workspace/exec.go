package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// MatchesCheckPrefix reports whether check IS one of prefixes, or extends one
// with a space-separated continuation (e.g. "go test ./..." extends "go test";
// "go testing" does not). The check_commands allowlist is the security boundary
// for gate checks, whether they were written by the planner (dag.validateChecks)
// or derived from the repo (vetting.deriveChecks) - both funnel through here.
func MatchesCheckPrefix(check string, prefixes []string) bool {
	for _, p := range prefixes {
		if check == p || strings.HasPrefix(check, p+" ") {
			return true
		}
	}
	return false
}

// SplitArgv splits s into an argv array WITHOUT invoking a shell: fields are
// whitespace-separated; a single- or double-quoted span is kept as one field
// (quotes stripped); a backslash escapes the next character outside single
// quotes. This is a deliberate SUBSET of shell word-splitting - no globbing,
// no variable expansion, no command substitution, since those are exactly the
// shell semantics argv-only execution exists to avoid - just enough to carry
// a quoted argument (a commit message, a grep pattern with spaces) through a
// single command string. Used by the trust gate's `checks` (an operator
// allowlist that must stay argv-only - internal/vetting/checks.go); run_command
// hands its command line to a real shell instead (RunShell) and never reaches
// this parser (#277).
func SplitArgv(s string) ([]string, error) {
	var argv []string
	var cur strings.Builder
	hasCur := false
	var quote rune
	esc := false
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			hasCur = true
			esc = false
		case quote == '\'':
			if r == '\'' {
				quote = 0
			} else {
				cur.WriteRune(r)
			}
		case quote == '"':
			switch r {
			case '"':
				quote = 0
			case '\\':
				esc = true
			default:
				cur.WriteRune(r)
			}
		case r == '\'' || r == '"':
			quote = r
			hasCur = true
		case r == '\\':
			esc = true
		case r == ' ' || r == '\t' || r == '\n':
			if hasCur {
				argv = append(argv, cur.String())
				cur.Reset()
				hasCur = false
			}
		default:
			cur.WriteRune(r)
			hasCur = true
		}
	}
	if quote != 0 {
		return nil, fmt.Errorf("workspace: unterminated %c quote in command", quote)
	}
	if esc {
		return nil, fmt.Errorf("workspace: trailing backslash in command")
	}
	if hasCur {
		argv = append(argv, cur.String())
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("workspace: empty command")
	}
	return argv, nil
}

// SplitPipeline splits s into pipeline stages on UNQUOTED `|` characters -
// the same quote/escape rules as SplitArgv (a `|` inside quotes, or escaped
// with a backslash, is a literal argument character) - then word-splits each
// stage through SplitArgv. An empty stage (leading/trailing/double pipe) is
// an error. A command with no pipe returns exactly one stage, so callers can
// use SplitPipeline unconditionally.
func SplitPipeline(s string) ([][]string, error) {
	var rawStages []string
	var cur strings.Builder
	var quote rune
	esc := false
	for _, r := range s {
		switch {
		case esc:
			cur.WriteRune(r)
			esc = false
		case quote != 0:
			if r == quote {
				quote = 0
			} else if quote == '"' && r == '\\' {
				esc = true
				cur.WriteRune(r) // keep for SplitArgv to interpret
				continue
			}
			cur.WriteRune(r)
		case r == '\\':
			esc = true
			cur.WriteRune(r) // keep for SplitArgv to interpret
		case r == '\'' || r == '"':
			quote = r
			cur.WriteRune(r)
		case r == '|':
			rawStages = append(rawStages, cur.String())
			cur.Reset()
		default:
			cur.WriteRune(r)
		}
	}
	rawStages = append(rawStages, cur.String())

	stages := make([][]string, 0, len(rawStages))
	for _, raw := range rawStages {
		if strings.TrimSpace(raw) == "" {
			return nil, fmt.Errorf("workspace: empty pipeline stage (leading, trailing, or doubled |)")
		}
		argv, err := SplitArgv(raw)
		if err != nil {
			return nil, err
		}
		stages = append(stages, argv)
	}
	return stages, nil
}

// ExecResult is one argv-only command execution's outcome. A non-zero
// ExitCode from the command itself is NOT a Go error (see RunArgv's doc) -
// it's a normal result the caller (run_command, or the gate's checks fold)
// inspects and reports to the model.
type ExecResult struct {
	ExitCode int
	Output   string // combined stdout+stderr, tail-truncated to caps.MaxOutputBytes
	TimedOut bool
}

// execEnvPath is the PATH every RunArgv child sees IN ITS OWN ENVIRONMENT -
// mirrors runGit's "hermetic, behavior never depends on the host" discipline
// (internal/tools/git.go's gitEnv), extended with the common toolchain
// locations the shipped check_commands prefixes need (go, npm/npx) for their
// OWN nested lookups (e.g. `go build` finding a linker, `npm test` finding a
// locally-installed runner). It does NOT affect how RunArgv finds argv[0]
// itself - that resolution uses the server process's real, ambient PATH (see
// exec.LookPath below), exactly like gitBinaryPath does for the git binary;
// scrubbing only the CHILD's env (no inherited secrets, no host-dependent
// PATH) is the isolation goal, not hiding the toolchain from itself.
const execEnvPath = "/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin"

// childPath is execEnvPath plus the operator-configured extra directories
// (Caps.ExtraPath / workspace.exec_path) - extras go FIRST so a configured
// toolchain (nvm's node) wins over a stale system one.
func childPath(caps Caps) string {
	if len(caps.ExtraPath) == 0 {
		return execEnvPath
	}
	return strings.Join(caps.ExtraPath, ":") + ":" + execEnvPath
}

// childHome is the $HOME every RunArgv/RunPipeline child sees: caps.HomeDir
// when the caller wired one up (the isolated per-user home OUTSIDE any cloned
// repo tree - see Jail.HomeDir), falling back to the task's own cwd only when
// unset (a caller/test that hasn't wired isolation up). Pinning HOME to dir
// was the LIVE bug: a coding task's cwd IS the target repo, so a child tool
// (npm, pip, …) writing its own cache to $HOME wrote it straight into the
// repo, and git_commit's add_all then swept the cache up as if it were part
// of the change.
func childHome(dir string, caps Caps) string {
	if caps.HomeDir != "" {
		return caps.HomeDir
	}
	return dir
}

// sortedEnvKeys orders caps.Env deterministically so the child's argv/env is
// reproducible across runs (and diffable in a test failure).
func sortedEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// childEnv is the full environment every RunArgv/RunPipeline child sees: the
// fixed PATH/HOME first, then the operator's workspace.env entries - config
// validation already rejects a PATH/HOME key there, so this never fights them.
// Under landlock, TMPDIR is pinned to the granted tmp dir too: Landlock can't
// remap /tmp the way bwrap does, so a tool that defaults to the real /tmp
// (go build's work dir, mktemp) must be TOLD where its writable tmp actually
// is, or it hits an ungranted path (observed: `go build` failing to create
// its work dir under the real /tmp).
func childEnv(dir string, caps Caps) []string {
	env := []string{"PATH=" + childPath(caps), "HOME=" + childHome(dir, caps)}
	if caps.Sandbox == SandboxLandlock {
		env = append(env, "TMPDIR="+landlockTmpDir(caps))
	}
	for _, k := range sortedEnvKeys(caps.Env) {
		env = append(env, k+"="+caps.Env[k])
	}
	return env
}

// ResolveExecutable finds argv[0]'s absolute path exactly as newChildCmd is
// about to exec it: a bare name (e.g. "go") resolves against the server's own
// ambient PATH via exec.LookPath, unchanged. A name containing a path
// separator (e.g. "./gradlew") is a REPO-relative executable and resolves
// against dir instead - never this process's cwd, which is what a plain
// exec.LookPath("./gradlew") would consult and, for any dir other than the
// process's own, silently fail to find (#638). Also shared by
// vetting.toolchainPresent so "will this run" and "did we predict it would
// run" can't drift apart.
func ResolveExecutable(dir, name string) (string, error) {
	if !strings.ContainsRune(name, '/') {
		return exec.LookPath(name)
	}
	p := name
	if !filepath.IsAbs(p) {
		p = filepath.Join(dir, p)
	}
	fi, err := os.Stat(p)
	if err != nil {
		return "", err
	}
	if fi.IsDir() || fi.Mode()&0o111 == 0 {
		return "", fmt.Errorf("not executable: %s", p)
	}
	return p, nil
}

// newChildCmd is the ONE place a child process is constructed - RunArgv and
// every stage of RunPipeline go through it, so the OS sandbox and the resource
// limits cannot be forgotten on one path and applied on the other.
//
// argv[0] is resolved via ResolveExecutable BEFORE the child's argv is
// constructed - exactly like gitBinaryPath does for `git` - so cmd.Env's PATH
// only governs the child's OWN nested lookups (see execEnvPath), never argv[0]
// itself. Under SandboxBwrap the resolved path is valid INSIDE the sandbox for
// a PATH toolchain (the system dirs and configured exec_path toolchains are
// bound at their own paths - see sandbox.go), but NOT yet for a repo-relative
// binary (e.g. "./gradlew"): bwrap remaps dir under SandboxWorkRoot, so the
// resolved host path won't exist inside the namespace. SandboxNone and
// SandboxLandlock (paths not remapped there) are unaffected.
func newChildCmd(ctx context.Context, dir string, argv []string, caps Caps) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("workspace: empty command")
	}
	bin, err := ResolveExecutable(dir, argv[0])
	if err != nil {
		return nil, fmt.Errorf("workspace: %q not found: %w", argv[0], err)
	}
	real := childArgv(dir, bin, argv, caps)
	cmd := exec.CommandContext(ctx, real[0], real[1:]...)
	cmd.Dir = dir
	// bwrap passes its own environment straight through to the sandboxed child,
	// so the scrub (no inherited secrets, a fixed PATH, the isolated HOME) holds
	// identically in both modes.
	cmd.Env = childEnv(dir, caps)
	// Own process group + kill the WHOLE group on cancel, and a WaitDelay backstop.
	// A real shell (RunShell) can leave a backgrounded grandchild that inherits our
	// stdout pipe; exec's default cancel kills only the direct child, so the
	// grandchild keeps the pipe open, the output-copy goroutine never sees EOF, and
	// cmd.Wait() blocks forever - even past the context timeout. Setpgid + a
	// group-kill Cancel reaps the whole tree on timeout; WaitDelay force-closes the
	// pipe (letting Wait return) if the process exits with a lingering writer, e.g.
	// a bare `cmd &`. (a live plan run wedged here - v0.5.2, the run_command shell.)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error {
		if cmd.Process == nil {
			return nil
		}
		return syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL) // negative pid = the process group
	}
	cmd.WaitDelay = childWaitDelay
	return cmd, nil
}

// childWaitDelay bounds how long Wait() will block on pipe I/O after the child
// has exited or been cancelled - a lingering grandchild holding stdout can't
// hang the call past this. A package var so a test can shorten it.
var childWaitDelay = 10 * time.Second

// RunArgv executes argv[0] with argv[1:] as a subprocess: exec.Command argv
// arrays ONLY, never a shell. cwd is pinned to dir (callers resolve it through
// a Jail first), the child's environment is scrubbed (execEnvPath +
// HOME=childHome(dir,caps)), a per-call timeout comes from caps (DefaultCaps
// when unset), and output is tail-capped. Shared by run_command
// (internal/tools) and the trust gate's per-node deterministic `checks`
// (internal/vetting/checks.go) - ONE runner, two consumers.
//
// The REAL containment is caps.Sandbox (SandboxBwrap: an OS mount/pid/user
// namespace - see sandbox.go), because the argv-only rule never was one: a
// child's ARGUMENTS are not path-checked, and `sh -c "…"` contains no rejected
// metacharacter. Not opening a shell here still matters (it keeps the model
// from expressing shell semantics quack never validated), but it is a habit
// guard; the namespace is the wall.
//
// err is reserved for a launch failure (binary not found, bad cwd) or a
// timeout; a non-zero exit from the command itself is reported via
// ExecResult.ExitCode with a nil error, so callers can surface "the test
// failed" to the model without it looking like a tool malfunction.
func RunArgv(ctx context.Context, dir string, argv []string, caps Caps) (ExecResult, error) {
	timeout := caps.Timeout
	if timeout <= 0 {
		timeout = DefaultCaps().Timeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd, err := newChildCmd(cctx, dir, argv, caps)
	if err != nil {
		return ExecResult{}, err
	}
	var buf bytes.Buffer
	cmd.Stdout = &buf
	cmd.Stderr = &buf

	runErr := cmd.Run()
	maxOut := caps.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = DefaultCaps().MaxOutputBytes
	}
	out := capTail(buf.String(), maxOut)
	exitCode := -1
	if cmd.ProcessState != nil {
		exitCode = cmd.ProcessState.ExitCode()
	}
	if cctx.Err() == context.DeadlineExceeded {
		return ExecResult{ExitCode: exitCode, Output: out, TimedOut: true},
			fmt.Errorf("workspace: run %v: timed out after %s", argv, timeout)
	}
	if errors.Is(runErr, exec.ErrWaitDelay) {
		// The command returned but left a background process holding our stdout
		// pipe; WaitDelay force-closed it and let us stop waiting rather than hang.
		// Surface the captured output plus a note (not an error) - the command
		// DID run, it just spawned something that outlives it.
		return ExecResult{ExitCode: exitCode, Output: out +
			"\n[run_command: the command left a background process still running; output above may be incomplete]"}, nil
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			// Not a plain non-zero exit (binary not found, permission denied,
			// …) - a real launch failure, surfaced as an error.
			return ExecResult{ExitCode: exitCode, Output: out}, fmt.Errorf("workspace: run %v: %w", argv, runErr)
		}
	}
	return ExecResult{ExitCode: exitCode, Output: out}, nil
}

// RunShell runs command as a REAL shell command line - `/bin/sh -c "<command>"`
// - unconditionally, whatever caps.Sandbox is set to (run_command's only
// runner; see internal/tools/run_command.go). Pipes, redirects, globs, `$(…)`,
// `&&`, quoting: all of it just works, because sh is doing it. sh is just
// another argv, so childArgv (sandbox.go) decides the boundary exactly as it
// does for any other child:
//
//   - caps.Sandbox == SandboxBwrap: sh runs INSIDE the bwrap namespace, and is
//     safe *because* the namespace is real - a shell cannot widen it: outside
//     its own cwd and its isolated $HOME nothing is writable, and outside the
//     read-only system view nothing EXISTS. `> /etc/passwd` from in there fails
//     like any other write to a read-only mount.
//   - anything else (SandboxNone): sh runs directly, with the server user's own
//     filesystem authority - exactly the authority a child already had on the
//     argv-only path this replaces, so offering it a shell adds no new reach,
//     only working `&&`/globs/redirects/`$(…)` (#277).
//
// RunArgv supplies everything else unchanged: the resolved binary, the
// scrubbed env, cwd=dir, the timeout, the output cap, and the "a non-zero exit
// is a result, not an error" contract.
func RunShell(ctx context.Context, dir, command string, caps Caps) (ExecResult, error) {
	if strings.TrimSpace(command) == "" {
		return ExecResult{}, fmt.Errorf("workspace: empty command")
	}
	return RunArgv(ctx, dir, []string{"sh", "-c", command}, caps)
}

// RunPipeline executes stages as a native pipeline: each stage is a plain
// argv process (exec.CommandContext - still never a shell), stage N's stdout
// connected to stage N+1's stdin by a real pipe. All stages share the jailed
// cwd, the scrubbed env, and ONE overall timeout (a deadline kills the whole
// pipeline). A single-stage pipeline delegates to RunArgv - one runner.
//
// Semantics chosen deliberately:
//   - Exit code is PIPEFAIL: the LAST non-zero stage's code; 0 only when every
//     stage succeeds (bash's `set -o pipefail` - a failing producer can't be
//     masked by a succeeding tail like `| head`).
//   - Output is the LAST stage's stdout plus every stage's stderr (each
//     prefixed with its stage when non-empty), tail-capped; failing stages are
//     named ("[pipeline] stage N of M (cmd) exited K") so the model sees WHICH
//     program failed, not just a code.
//   - A SIGPIPE-shaped death (a producer killed because its consumer exited
//     early, e.g. `grep -r … | head -5`) is still just an exit code here; the
//     model sees it named and can judge whether it mattered.
//
// Like RunArgv, err is reserved for launch failures and timeouts; stage exit
// codes are results, not errors.
func RunPipeline(ctx context.Context, dir string, stages [][]string, caps Caps) (ExecResult, error) {
	if len(stages) == 0 {
		return ExecResult{}, fmt.Errorf("workspace: empty pipeline")
	}
	if len(stages) == 1 {
		return RunArgv(ctx, dir, stages[0], caps)
	}
	timeout := caps.Timeout
	if timeout <= 0 {
		timeout = DefaultCaps().Timeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	// Build every stage up front (newChildCmd resolves the binary and applies
	// the sandbox + limits - see its doc) so a missing program is one clean
	// error before anything starts. Each stage is its OWN sandbox; the pipes
	// between them are inherited file descriptors, which cross the namespace
	// boundary exactly as they cross a process boundary.
	cmds := make([]*exec.Cmd, len(stages))
	stderrs := make([]*bytes.Buffer, len(stages)) // one buffer per stage: exec copies stderr on its own goroutine, so a shared buffer would race
	for i, argv := range stages {
		cmd, err := newChildCmd(cctx, dir, argv, caps)
		if err != nil {
			return ExecResult{}, err
		}
		stderrs[i] = &bytes.Buffer{}
		cmd.Stderr = stderrs[i]
		cmds[i] = cmd
	}
	var stdout bytes.Buffer
	cmds[len(cmds)-1].Stdout = &stdout
	for i := 1; i < len(cmds); i++ {
		pipe, err := cmds[i-1].StdoutPipe()
		if err != nil {
			return ExecResult{}, fmt.Errorf("workspace: pipeline pipe: %w", err)
		}
		cmds[i].Stdin = pipe
	}

	for i, cmd := range cmds {
		if err := cmd.Start(); err != nil {
			// Reap anything already started so nothing leaks.
			for _, prev := range cmds[:i] {
				_ = prev.Process.Kill()
				_ = prev.Wait()
			}
			return ExecResult{}, fmt.Errorf("workspace: start %v: %w", stages[i], err)
		}
	}

	// Wait in pipeline order. A stage's non-zero exit (including being killed
	// because its consumer closed the pipe early) is a result, not an error;
	// only non-exit failures abort.
	exitCode := 0
	var failNotes []string
	for i, cmd := range cmds {
		waitErr := cmd.Wait()
		code := -1
		if cmd.ProcessState != nil {
			code = cmd.ProcessState.ExitCode()
		}
		if waitErr != nil {
			var exitErr *exec.ExitError
			if !errors.As(waitErr, &exitErr) && cctx.Err() != context.DeadlineExceeded {
				return ExecResult{}, fmt.Errorf("workspace: run %v: %w", stages[i], waitErr)
			}
		}
		if code != 0 {
			exitCode = code // pipefail: last non-zero wins
			failNotes = append(failNotes, fmt.Sprintf("[pipeline] stage %d of %d (%s) exited %d",
				i+1, len(cmds), strings.Join(stages[i], " "), code))
		}
	}

	var out strings.Builder
	out.WriteString(stdout.String())
	for i, eb := range stderrs {
		if eb.Len() == 0 {
			continue
		}
		fmt.Fprintf(&out, "\n[stage %d stderr] %s", i+1, strings.TrimRight(eb.String(), "\n"))
	}
	for _, note := range failNotes {
		out.WriteString("\n")
		out.WriteString(note)
	}
	maxOut := caps.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = DefaultCaps().MaxOutputBytes
	}
	res := ExecResult{ExitCode: exitCode, Output: capTail(out.String(), maxOut)}
	if cctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		return res, fmt.Errorf("workspace: pipeline timed out after %s", timeout)
	}
	return res, nil
}

// capTail truncates s to max bytes, keeping the TAIL - a compiler/test
// failure's most useful line is usually its last one (the assertion, the
// error summary), unlike git's own capOutput (internal/tools/git.go), which
// truncates from the head.
func capTail(s string, max int64) string {
	if int64(len(s)) <= max || max <= 0 {
		return s
	}
	return "... (truncated)\n" + s[int64(len(s))-max:]
}
