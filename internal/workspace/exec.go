package workspace

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"
)

// MatchesCheckPrefix reports whether check is one of prefixes or extends one with a space.
func MatchesCheckPrefix(check string, prefixes []string) bool {
	for _, p := range prefixes {
		if check == p || strings.HasPrefix(check, p+" ") {
			return true
		}
	}
	return false
}

// SplitArgv splits s into argv (whitespace, quotes, backslash escapes), no shell.
func SplitArgv(s string) ([]string, error) {
	var argv []string
	var st argvState
	for _, r := range s {
		if tok, ok := st.step(r); ok {
			argv = append(argv, tok)
		}
	}
	if st.quote != 0 {
		return nil, fmt.Errorf("workspace: unterminated %c quote in command", st.quote)
	}
	if st.esc {
		return nil, fmt.Errorf("workspace: trailing backslash in command")
	}
	if st.hasCur {
		argv = append(argv, st.cur.String())
	}
	if len(argv) == 0 {
		return nil, fmt.Errorf("workspace: empty command")
	}
	return argv, nil
}

// argvState: the accumulator state for one SplitArgv pass.
type argvState struct {
	cur    strings.Builder
	hasCur bool
	quote  rune
	esc    bool
}

// step returns the token whitespace just closed, if any.
func (st *argvState) step(r rune) (string, bool) {
	switch {
	case st.esc:
		st.cur.WriteRune(r)
		st.hasCur = true
		st.esc = false
	case st.quote == '\'':
		if r == '\'' {
			st.quote = 0
		} else {
			st.cur.WriteRune(r)
		}
	case st.quote == '"':
		if r == '"' {
			st.quote = 0
		} else if r == '\\' {
			st.esc = true
		} else {
			st.cur.WriteRune(r)
		}
	case r == '\'' || r == '"':
		st.quote = r
		st.hasCur = true
	case r == '\\':
		st.esc = true
	case r == ' ' || r == '\t' || r == '\n':
		if st.hasCur {
			tok := st.cur.String()
			st.cur.Reset()
			st.hasCur = false
			return tok, true
		}
	default:
		st.cur.WriteRune(r)
		st.hasCur = true
	}
	return "", false
}

// SplitPipeline splits s on unquoted `|`, then word-splits each stage via SplitArgv.
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

// ExecResult is one argv-only command execution's outcome. Non-zero exit is not a Go error.
type ExecResult struct {
	ExitCode int
	Output   string // combined stdout+stderr, tail-truncated to caps.MaxOutputBytes
	TimedOut bool
}

// execEnvPath is the hermetic child PATH; a var so tests can point it at a fixture dir.
var execEnvPath = "/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin"

// childPath prepends Caps.ExtraPath to execEnvPath so configured toolchains win.
func childPath(caps Caps) string {
	if len(caps.ExtraPath) == 0 {
		return execEnvPath
	}
	return strings.Join(caps.ExtraPath, ":") + ":" + execEnvPath
}

// childHome: a HOME inside the repo gets tool caches (npm) swept into commits.
func childHome(dir string, caps Caps) string {
	if caps.HomeDir != "" {
		return caps.HomeDir
	}
	return dir
}

func sortedEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

func childEnv(dir string, caps Caps) []string {
	env := []string{"PATH=" + childPath(caps), "HOME=" + childHome(dir, caps)}
	if caps.Sandbox == SandboxLandlock {
		tmp := landlockTmpDir(caps)
		// GOTMPDIR must track TMPDIR: unset, Go builds in os.TempDir(), which the jail doesn't grant.
		env = append(env, "TMPDIR="+tmp, "GOTMPDIR="+tmp)
	}
	if opts := SandboxJavaToolOptions(caps); opts != "" {
		env = append(env, "JAVA_TOOL_OPTIONS="+opts)
	}
	for _, k := range sortedEnvKeys(caps.Env) {
		env = append(env, k+"="+caps.Env[k])
	}
	// Appended last (last duplicate wins) to override caps.Env's read-only preseeded GOMODCACHE:
	// it must be writable even for an offline `go test`.
	env = append(env,
		"GOMODCACHE="+EnsureWritableGoModCache(childHome(dir, caps)),
		"GOCACHE="+filepath.Join(childHome(dir, caps), ".cache", "go-build"),
		"GOFLAGS=-mod=mod",
		"GOTOOLCHAIN=local",
	)
	return env
}

// ResolveExecutable finds argv[0]: bare name via LookPath, path-containing name relative to dir.
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

// newChildCmd is the single construction point for child processes, so sandboxing is never missed.
func newChildCmd(ctx context.Context, dir string, argv []string, caps Caps) (*exec.Cmd, error) {
	if len(argv) == 0 {
		return nil, fmt.Errorf("workspace: empty command")
	}
	if dir != "" {
		if fi, statErr := os.Stat(dir); statErr != nil || !fi.IsDir() {
			// Setpgid below skips Go's os/exec chdir precheck, so a missing dir
			// would otherwise surface as a fork/exec error naming the binary.
			return nil, fmt.Errorf("workspace: workdir %q does not exist", dir)
		}
	}
	bin, err := ResolveExecutable(dir, argv[0])
	if err != nil {
		return nil, fmt.Errorf("workspace: %q not found: %w", argv[0], err)
	}
	real := childArgv(dir, bin, argv, caps)
	cmd := exec.CommandContext(ctx, real[0], real[1:]...)
	cmd.Dir = dir
	// bwrap passes its environment straight through, so the scrub holds identically in both modes.
	cmd.Env = childEnv(dir, caps)
	// Own process group + StopChild + WaitDelay to prevent grandchild hangs.
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.Cancel = func() error { return StopChild(cmd) }
	cmd.WaitDelay = childWaitDelay
	return cmd, nil
}

// reapGrace bounds how long a __reap wrapper gets after reapStopSignal; it exceeds reapSweepBudget.
var reapGrace = 8 * time.Second

// reapStopSignal is StopChild's private hard stop for a __reap wrapper, which forwards SIGTERM/SIGINT/
// SIGHUP/SIGQUIT to its target instead so a graceful signal stays graceful.
const reapStopSignal = syscall.SIGUSR2

// StopChild tears down a child built from WrapArgv/childArgv argv. A __reap wrapper gets
// reapStopSignal and kills every descendant itself; otherwise (or if it overruns) killGroup.
func StopChild(cmd *exec.Cmd) error {
	p := cmd.Process
	if p == nil {
		return nil
	}
	if len(cmd.Args) < 2 || cmd.Args[1] != ReapArg {
		return killGroup(p)
	}
	grace := reapGrace
	time.AfterFunc(grace, func() {
		if p.Signal(syscall.Signal(0)) != nil {
			return // exited and reaped
		}
		if killGroup(p) == nil {
			slog.Error("sandbox reaper still running after its stop signal; SIGKILLed it and its process group, descendants outside the group may survive",
				"component", "workspace", "pid", p.Pid, "grace", grace)
		}
	})
	return p.Signal(reapStopSignal)
}

// killGroup SIGKILLs p's process group, or p alone when it leads no group (no Setpgid).
func killGroup(p *os.Process) error {
	if syscall.Kill(-p.Pid, syscall.SIGKILL) == nil {
		return nil
	}
	return p.Kill()
}

// childWaitDelay bounds Wait()'s pipe I/O block after child exit; a var for tests.
var childWaitDelay = 10 * time.Second

// RunArgv executes argv via exec.Command (no shell). Non-zero exit returns via ExitCode, not error.
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
		return ExecResult{ExitCode: exitCode, Output: out +
			"\n[run_command: the command left a background process still running; output above may be incomplete]"}, nil
	}
	if runErr != nil {
		var exitErr *exec.ExitError
		if !errors.As(runErr, &exitErr) {
			return ExecResult{ExitCode: exitCode, Output: out}, fmt.Errorf("workspace: run %v: %w", argv, runErr)
		}
	}
	return ExecResult{ExitCode: exitCode, Output: out + fileSizeLimitNote(cmd.ProcessState, caps.Limits)}, nil
}

// fileSizeLimitNote: a bare SIGXFSZ reads as the command's own fault. Only FSIZE is detectable;
// RLIMIT_AS surfaces as an ENOMEM inside the child.
func fileSizeLimitNote(st *os.ProcessState, lim Limits) string {
	if st == nil || lim.FileSizeMB <= 0 {
		return ""
	}
	ws, ok := st.Sys().(syscall.WaitStatus)
	if !ok || !ws.Signaled() || ws.Signal() != syscall.SIGXFSZ {
		return ""
	}
	return fmt.Sprintf("\n[workspace: killed by SIGXFSZ - it tried to write a file larger than "+
		"workspace.limits.max_file_size_mb (%dMB). Raise that limit or have the command write less.]", lim.FileSizeMB)
}

// RunPipeline executes argv stages with real pipes. Exit code is pipefail: last non-zero stage.
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

	cmds, stderrs, stdout, err := buildPipelineCmds(cctx, dir, stages, caps)
	if err != nil {
		return ExecResult{}, err
	}
	if err := startPipelineCmds(cmds, stages); err != nil {
		return ExecResult{}, err
	}
	exitCode, failNotes, err := waitPipelineCmds(cctx, cmds, stages, caps)
	if err != nil {
		return ExecResult{}, err
	}

	maxOut := caps.MaxOutputBytes
	if maxOut <= 0 {
		maxOut = DefaultCaps().MaxOutputBytes
	}
	res := ExecResult{ExitCode: exitCode, Output: capTail(pipelineOutput(stdout.String(), stderrs, failNotes), maxOut)}
	if cctx.Err() == context.DeadlineExceeded {
		res.TimedOut = true
		return res, fmt.Errorf("workspace: pipeline timed out after %s", timeout)
	}
	return res, nil
}

// buildPipelineCmds builds every stage up front so a missing binary fails before anything starts.
func buildPipelineCmds(cctx context.Context, dir string, stages [][]string, caps Caps) ([]*exec.Cmd, []*bytes.Buffer, *bytes.Buffer, error) {
	cmds := make([]*exec.Cmd, len(stages))
	stderrs := make([]*bytes.Buffer, len(stages)) // one per stage: a shared buffer would race
	for i, argv := range stages {
		cmd, err := newChildCmd(cctx, dir, argv, caps)
		if err != nil {
			return nil, nil, nil, err
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
			return nil, nil, nil, fmt.Errorf("workspace: pipeline pipe: %w", err)
		}
		cmds[i].Stdin = pipe
	}
	return cmds, stderrs, &stdout, nil
}

func startPipelineCmds(cmds []*exec.Cmd, stages [][]string) error {
	for i, cmd := range cmds {
		if err := cmd.Start(); err != nil {
			for _, prev := range cmds[:i] {
				_ = StopChild(prev)
				_ = prev.Wait()
			}
			return fmt.Errorf("workspace: start %v: %w", stages[i], err)
		}
	}
	return nil
}

// waitPipelineCmds: a non-zero exit is a result, not an error (pipefail: last non-zero wins).
func waitPipelineCmds(cctx context.Context, cmds []*exec.Cmd, stages [][]string, caps Caps) (int, []string, error) {
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
				return 0, nil, fmt.Errorf("workspace: run %v: %w", stages[i], waitErr)
			}
		}
		if code != 0 {
			exitCode = code // pipefail: last non-zero wins
			failNotes = append(failNotes, fmt.Sprintf("[pipeline] stage %d of %d (%s) exited %d%s",
				i+1, len(cmds), strings.Join(stages[i], " "), code, fileSizeLimitNote(cmd.ProcessState, caps.Limits)))
		}
	}
	return exitCode, failNotes, nil
}

// pipelineOutput: stdout plus each stage's stderr and the pipefail failure notes.
func pipelineOutput(stdout string, stderrs []*bytes.Buffer, failNotes []string) string {
	var out strings.Builder
	out.WriteString(stdout)
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
	return out.String()
}

// capTail truncates s to max bytes keeping the tail (most useful for assertion errors).
func capTail(s string, max int64) string {
	if int64(len(s)) <= max || max <= 0 {
		return s
	}
	return "... (truncated)\n" + s[int64(len(s))-max:]
}
