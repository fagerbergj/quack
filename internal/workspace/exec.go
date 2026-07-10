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
// signals shell-interpretation intent (pipe, background, redirect, command/
// variable substitution, subshell). Rejected wherever a command string is
// accepted for jailed argv-only execution — run_command's `command` arg
// (internal/tools) and a plan node's `checks` (internal/dag's planner) — so a
// shell escape is unexpressible, not merely filtered by heuristic: RunArgv
// never opens a shell to interpret them either way, but rejecting them at
// parse time gives the model (or the planner) a clear, early error instead of
// a confusing "argument" to some other program.
const shellMetachars = "|&;$<>`()"

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

// RunArgv executes argv[0] with argv[1:] as a subprocess: exec.Command argv
// arrays ONLY, never a shell. The caller has already rejected shell
// metacharacters and split the command itself (ContainsShellMetachar /
// SplitArgv) — this is the LAST wall: even a validated argv never touches
// /bin/sh. cwd is pinned to dir (callers resolve it through a Jail first), the
// child's environment is scrubbed (execEnvPath + HOME=dir), a per-call
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
	cmd.Env = []string{"PATH=" + execEnvPath, "HOME=" + dir}
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
