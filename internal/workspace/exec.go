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

// execEnvPath is the hermetic PATH every RunArgv child sees.
const execEnvPath = "/usr/local/go/bin:/usr/local/bin:/usr/bin:/bin"

// childPath prepends Caps.ExtraPath to execEnvPath so configured toolchains win.
func childPath(caps Caps) string {
	if len(caps.ExtraPath) == 0 {
		return execEnvPath
	}
	return strings.Join(caps.ExtraPath, ":") + ":" + execEnvPath
}

// childHome returns caps.HomeDir or dir. Repo-relative HOME was a live bug (npm cache swept into commit).
func childHome(dir string, caps Caps) string {
	if caps.HomeDir != "" {
		return caps.HomeDir
	}
	return dir
}

// sortedEnvKeys orders caps.Env deterministically for reproducible child env.
func sortedEnvKeys(env map[string]string) []string {
	keys := make([]string, 0, len(env))
	for k := range env {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	return keys
}

// childEnv builds the child's environment: fixed PATH/HOME, workspace.env entries, landlock TMPDIR.
func childEnv(dir string, caps Caps) []string {
	env := []string{"PATH=" + childPath(caps), "HOME=" + childHome(dir, caps)}
	if caps.Sandbox == SandboxLandlock {
		tmp := landlockTmpDir(caps)
		// GOTMPDIR must track TMPDIR: unset, Go's build work dir defaults to
		// os.TempDir(), which the jail doesn't grant (#936).
		env = append(env, "TMPDIR="+tmp, "GOTMPDIR="+tmp)
	}
	if opts := SandboxJavaToolOptions(caps); opts != "" {
		env = append(env, "JAVA_TOOL_OPTIONS="+opts)
	}
	for _, k := range sortedEnvKeys(caps.Env) {
		env = append(env, k+"="+caps.Env[k])
	}
	// Appended LAST (exec.Cmd.Env: last value wins for a duplicate key) so
	// these win over caps.Env's GOMODCACHE (the #940 preseed, read-only) - same
	// reasoning and helper as acp.Agent.spawnEnv (#954): GOMODCACHE must be
	// writable even for an offline `go test`.
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

// newChildCmd is the single construction point for child processes so sandboxing is never missed.
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
	// Own process group + group kill + WaitDelay to prevent grandchild hangs.
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

// childWaitDelay bounds Wait() pipe I/O block after child exit. Package var for tests.
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

// fileSizeLimitNote names workspace.limits when SIGXFSZ killed the child - a
// bare "signal: file size limit exceeded" reads as the command's own fault
// (#798, where a limit quack imposed cost four bisect cycles to attribute).
// Only FSIZE is detectable: exceeding RLIMIT_AS fails an allocation INSIDE the
// child with ENOMEM, so it surfaces as whatever that child makes of it.
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

	// Build every stage up front so a missing binary fails before anything starts.
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

	// Wait in pipeline order. Non-zero exit is a result, not an error.
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
			failNotes = append(failNotes, fmt.Sprintf("[pipeline] stage %d of %d (%s) exited %d%s",
				i+1, len(cmds), strings.Join(stages[i], " "), code, fileSizeLimitNote(cmd.ProcessState, caps.Limits)))
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

// capTail truncates s to max bytes keeping the tail (most useful for assertion errors).
func capTail(s string, max int64) string {
	if int64(len(s)) <= max || max <= 0 {
		return s
	}
	return "... (truncated)\n" + s[int64(len(s))-max:]
}
