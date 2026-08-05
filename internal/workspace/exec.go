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

// SplitArgv splits s into argv WITHOUT invoking a shell: whitespace-separated
// fields, quoted spans as one field, backslash escapes outside single quotes.
// Deliberately a SUBSET of shell word-splitting (no globbing/expansion/
// substitution) because it feeds the trust gate's argv-only `checks`
// allowlist (internal/vetting/checks.go).
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
// hermetic like gitEnv (internal/tools/git.go), extended with common
// toolchain dirs (go, npm/npx) so checks can find their OWN nested tools.
// It does NOT affect how RunArgv resolves argv[0] itself - that uses the
// server's real ambient PATH (see exec.LookPath below), same as gitBinaryPath.
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
	if opts := SandboxJavaToolOptions(caps); opts != "" {
		env = append(env, "JAVA_TOOL_OPTIONS="+opts)
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

// newChildCmd is the ONE place a child process is constructed, so sandboxing
// and resource limits can't be applied on one path and forgotten on another.
// argv[0] resolves via ResolveExecutable before argv is built, so PATH never
// governs it - except under SandboxBwrap, where a repo-relative binary (e.g.
// "./gradlew") resolves to a host path bwrap's remap won't find in the namespace.
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
	// A shell child (e.g. `sh -c "cmd &"`) can leave a backgrounded grandchild that
	// inherits our stdout pipe; exec's default cancel kills only the direct child,
	// so the grandchild keeps the pipe open, the output-copy goroutine never sees
	// EOF, and cmd.Wait() blocks forever - even past the context timeout. Setpgid +
	// a group-kill Cancel reaps the whole tree on timeout; WaitDelay force-closes
	// the pipe (letting Wait return) if the process exits with a lingering writer.
	// (a live plan run wedged here - v0.5.2.)
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

// RunArgv executes argv[0] with argv[1:] via exec.Command arrays ONLY, never
// a shell; cwd pinned, env scrubbed, timeout-bounded, output tail-capped.
// caps.Sandbox is the REAL containment - argv-only is a habit guard since
// arguments aren't path-checked, not a boundary. A non-zero exit reports via
// ExitCode with a nil error; err itself means launch failure or timeout.
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

// RunPipeline executes stages as a native pipeline (real pipes between plain
// argv processes, never a shell); a single stage delegates to RunArgv. Exit
// code is PIPEFAIL: the LAST non-zero stage, 0 only if all succeed. Output is
// the last stage's stdout plus every stage's stderr, with each failing stage
// named ("stage N of M (cmd) exited K") so the model sees WHICH program failed.
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
