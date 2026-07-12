package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
	"strings"
)

// shellMetachars are the characters whose presence in a raw command string
// signals shell-interpretation intent (backgrounding, command chaining,
// redirects, command/variable substitution, subshells). Rejected wherever a
// command string is accepted for jailed argv-only execution — run_command's
// `command` arg (internal/tools) and a plan node's `checks` (internal/dag's
// planner) — so a shell escape is unexpressible, not merely filtered by
// heuristic: RunArgv/RunPipeline never open a shell to interpret them either
// way, but rejecting them at parse time gives the model (or the planner) a
// clear, early error instead of a confusing "argument" to some other program.
//
// `|` is deliberately NOT in this set: pipes don't need a shell — SplitPipeline
// splits a pipeline on unquoted `|` and RunPipeline chains the stages as plain
// argv processes connected by real pipes. The rest stay unexpressible.
const shellMetachars = "&;$<>`()"

// ContainsShellMetachar reports whether s contains a shell metacharacter. See
// the shellMetachars doc comment for why these are rejected outright.
func ContainsShellMetachar(s string) bool {
	return strings.ContainsAny(s, shellMetachars)
}

// SplitArgv splits s into an argv array WITHOUT invoking a shell: fields are
// whitespace-separated; a single- or double-quoted span is kept as one field
// (quotes stripped); a backslash escapes the next character outside single
// quotes. This is a deliberate SUBSET of shell word-splitting — no globbing,
// no variable expansion, no command substitution, since those are exactly the
// shell semantics argv-only execution exists to avoid — just enough to carry
// a quoted argument (a commit message, a grep pattern with spaces) through a
// single command string. The ONLY place a run_command/`checks` string is ever
// turned into an argv array (internal/tools/run_command.go,
// internal/vetting/checks.go).
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

// SplitPipeline splits s into pipeline stages on UNQUOTED `|` characters —
// the same quote/escape rules as SplitArgv (a `|` inside quotes, or escaped
// with a backslash, is a literal argument character) — then word-splits each
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
// ExitCode from the command itself is NOT a Go error (see RunArgv's doc) —
// it's a normal result the caller (run_command, or the gate's checks fold)
// inspects and reports to the model.
type ExecResult struct {
	ExitCode int
	Output   string // combined stdout+stderr, tail-truncated to caps.MaxOutputBytes
	TimedOut bool
}

// execEnvPath is the PATH every RunArgv child sees IN ITS OWN ENVIRONMENT —
// mirrors runGit's "hermetic, behavior never depends on the host" discipline
// (internal/tools/git.go's gitEnv), extended with the common toolchain
// locations the shipped check_commands prefixes need (go, npm/npx) for their
// OWN nested lookups (e.g. `go build` finding a linker, `npm test` finding a
// locally-installed runner). It does NOT affect how RunArgv finds argv[0]
// itself — that resolution uses the server process's real, ambient PATH (see
// exec.LookPath below), exactly like gitBinaryPath does for the git binary;
// scrubbing only the CHILD's env (no inherited secrets, no host-dependent
// PATH) is the isolation goal, not hiding the toolchain from itself.
const execEnvPath = "/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin"

// childPath is execEnvPath plus the operator-configured extra directories
// (Caps.ExtraPath / workspace.exec_path) — extras go FIRST so a configured
// toolchain (nvm's node) wins over a stale system one.
func childPath(caps Caps) string {
	if len(caps.ExtraPath) == 0 {
		return execEnvPath
	}
	return strings.Join(caps.ExtraPath, ":") + ":" + execEnvPath
}

// childHome is the $HOME every RunArgv/RunPipeline child sees: caps.HomeDir
// when the caller wired one up (the isolated per-user home OUTSIDE any cloned
// repo tree — see Jail.HomeDir), falling back to the task's own cwd only when
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

// RunArgv executes argv[0] with argv[1:] as a subprocess: exec.Command argv
// arrays ONLY, never a shell. The caller has already rejected shell
// metacharacters and split the command itself (ContainsShellMetachar /
// SplitArgv) — this is the LAST wall: even a validated argv never touches
// /bin/sh. cwd is pinned to dir (callers resolve it through a Jail first), the
// child's environment is scrubbed (execEnvPath + HOME=childHome(dir,caps)), a per-call
// timeout comes from caps (DefaultCaps when unset), and output is
// tail-capped. Shared by run_command (internal/tools) and the trust gate's
// per-node deterministic `checks` (internal/vetting/checks.go) — ONE runner,
// two consumers.
//
// err is reserved for a launch failure (binary not found, bad cwd) or a
// timeout; a non-zero exit from the command itself is reported via
// ExecResult.ExitCode with a nil error, so callers can surface "the test
// failed" to the model without it looking like a tool malfunction.
func RunArgv(ctx context.Context, dir string, argv []string, caps Caps) (ExecResult, error) {
	if len(argv) == 0 {
		return ExecResult{}, fmt.Errorf("workspace: empty command")
	}
	// Resolve the binary via the server process's own (ambient) PATH — exactly
	// like gitBinaryPath does for `git` — BEFORE constructing the child's argv;
	// exec.Command would otherwise resolve a bare name using the CALLING
	// process's PATH regardless of what cmd.Env sets, silently ignoring our
	// scrub. Passing the already-resolved absolute path means no further
	// lookup happens, so cmd.Env's PATH only governs the child's OWN nested
	// lookups (see execEnvPath).
	bin, err := exec.LookPath(argv[0])
	if err != nil {
		return ExecResult{}, fmt.Errorf("workspace: %q not found: %w", argv[0], err)
	}
	timeout := caps.Timeout
	if timeout <= 0 {
		timeout = DefaultCaps().Timeout
	}
	cctx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()

	cmd := exec.CommandContext(cctx, bin, argv[1:]...)
	cmd.Dir = dir
	cmd.Env = []string{"PATH=" + childPath(caps), "HOME=" + childHome(dir, caps)}
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
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			// Not a plain non-zero exit (binary not found, permission denied,
			// …) — a real launch failure, surfaced as an error.
			return ExecResult{ExitCode: exitCode, Output: out}, fmt.Errorf("workspace: run %v: %w", argv, runErr)
		}
	}
	return ExecResult{ExitCode: exitCode, Output: out}, nil
}

// RunPipeline executes stages as a native pipeline: each stage is a plain
// argv process (exec.CommandContext — still never a shell), stage N's stdout
// connected to stage N+1's stdin by a real pipe. All stages share the jailed
// cwd, the scrubbed env, and ONE overall timeout (a deadline kills the whole
// pipeline). A single-stage pipeline delegates to RunArgv — one runner.
//
// Semantics chosen deliberately:
//   - Exit code is PIPEFAIL: the LAST non-zero stage's code; 0 only when every
//     stage succeeds (bash's `set -o pipefail` — a failing producer can't be
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

	// Resolve every binary up front (see RunArgv's LookPath rationale) so a
	// missing program is one clean error before anything starts.
	cmds := make([]*exec.Cmd, len(stages))
	stderrs := make([]*bytes.Buffer, len(stages)) // one buffer per stage: exec copies stderr on its own goroutine, so a shared buffer would race
	for i, argv := range stages {
		if len(argv) == 0 {
			return ExecResult{}, fmt.Errorf("workspace: empty pipeline stage")
		}
		bin, err := exec.LookPath(argv[0])
		if err != nil {
			return ExecResult{}, fmt.Errorf("workspace: %q not found: %w", argv[0], err)
		}
		cmd := exec.CommandContext(cctx, bin, argv[1:]...)
		cmd.Dir = dir
		cmd.Env = []string{"PATH=" + childPath(caps), "HOME=" + childHome(dir, caps)}
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

// capTail truncates s to max bytes, keeping the TAIL — a compiler/test
// failure's most useful line is usually its last one (the assertion, the
// error summary), unlike git's own capOutput (internal/tools/git.go), which
// truncates from the head.
func capTail(s string, max int64) string {
	if int64(len(s)) <= max || max <= 0 {
		return s
	}
	return "... (truncated)\n" + s[int64(len(s))-max:]
}
