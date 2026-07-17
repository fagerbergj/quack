package acp

import (
	"context"
	"fmt"
	"log/slog"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	sdk "github.com/coder/acp-go-sdk"
)

// contextT keeps the sdk.Client method signatures readable below.
type contextT = context.Context

// procHandle is one running ACP subprocess with its client-side connection.
type procHandle struct {
	cmd     *exec.Cmd
	conn    *sdk.ClientSideConnection
	updates chan sdk.SessionUpdate
	stop    chan struct{} // closed on shutdown so the update pump never blocks forever
	stderr  *tailBuffer
	once    sync.Once
}

// start spawns the agent subprocess rooted at cwd and wires the ACP connection.
func (a *Agent) start(cwd string) (*procHandle, error) {
	h := &procHandle{
		updates: make(chan sdk.SessionUpdate, 64),
		stop:    make(chan struct{}),
		stderr:  &tailBuffer{max: 4096},
	}
	cmd := exec.Command(a.opts.Command[0], a.opts.Command[1:]...)
	cmd.Dir = cwd
	cmd.Env = append([]string{
		"PATH=" + os.Getenv("PATH"),
		"HOME=" + a.opts.Home,
		"TMPDIR=" + os.TempDir(),
		"NO_COLOR=1",
	}, a.opts.Env...)
	// Own process group + group kill + WaitDelay: the exact hang class from the
	// v0.5.2 run_command incident — a grandchild holding our stdout pipe keeps
	// Wait blocked forever unless the whole group dies and the pipe is
	// force-closed (mirrors workspace.newChildCmd).
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	cmd.WaitDelay = 10 * time.Second

	stdin, err := cmd.StdinPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stdin: %w", err)
	}
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return nil, fmt.Errorf("acp: stdout: %w", err)
	}
	cmd.Stderr = h.stderr
	if err := cmd.Start(); err != nil {
		return nil, fmt.Errorf("acp: start %q: %w", strings.Join(a.opts.Command, " "), err)
	}
	h.cmd = cmd
	h.conn = sdk.NewClientSideConnection(&clientHandler{h: h}, stdin, stdout)
	return h, nil
}

// close kills the subprocess's whole process group and reaps it. Idempotent.
func (h *procHandle) close(log *slog.Logger) {
	h.once.Do(func() {
		close(h.stop)
		if h.cmd.Process != nil {
			_ = syscall.Kill(-h.cmd.Process.Pid, syscall.SIGKILL)
		}
		if err := h.cmd.Wait(); err != nil && log != nil {
			log.Debug("acp subprocess exit", "err", err, "stderr", h.stderr.String())
		}
	})
}

// stderrTail renders the captured stderr tail for error messages ("" if empty).
func (h *procHandle) stderrTail() string {
	s := strings.TrimSpace(h.stderr.String())
	if s == "" {
		return ""
	}
	return "\nagent stderr: " + s
}

// clientHandler implements the ACP client side. quack advertises no fs/terminal
// capabilities, so those methods only fire on a misbehaving agent — they refuse.
type clientHandler struct{ h *procHandle }

var _ sdk.Client = (*clientHandler)(nil)

func (c *clientHandler) SessionUpdate(ctx contextT, n sdk.SessionNotification) error {
	select {
	case c.h.updates <- n.Update:
	case <-c.h.stop:
	case <-ctx.Done():
	}
	return nil
}

// RequestPermission auto-answers: headless policy is configured on the agent
// side ("permission": "allow" with git push denied — see serve's
// opencodeConfigEnv), so asks should be rare; anything that still asks gets the
// first allow option, or is rejected when none exists.
func (c *clientHandler) RequestPermission(ctx contextT, p sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	for _, kind := range []sdk.PermissionOptionKind{sdk.PermissionOptionKindAllowOnce, sdk.PermissionOptionKindAllowAlways} {
		for _, o := range p.Options {
			if o.Kind == kind {
				return sdk.RequestPermissionResponse{Outcome: sdk.RequestPermissionOutcome{
					Selected: &sdk.RequestPermissionOutcomeSelected{OptionId: o.OptionId},
				}}, nil
			}
		}
	}
	return sdk.RequestPermissionResponse{Outcome: sdk.RequestPermissionOutcome{
		Cancelled: &sdk.RequestPermissionOutcomeCancelled{},
	}}, nil
}

func (c *clientHandler) ReadTextFile(ctx contextT, p sdk.ReadTextFileRequest) (sdk.ReadTextFileResponse, error) {
	return sdk.ReadTextFileResponse{}, fmt.Errorf("fs/read_text_file not supported (capability not advertised)")
}

func (c *clientHandler) WriteTextFile(ctx contextT, p sdk.WriteTextFileRequest) (sdk.WriteTextFileResponse, error) {
	return sdk.WriteTextFileResponse{}, fmt.Errorf("fs/write_text_file not supported (capability not advertised)")
}

func (c *clientHandler) CreateTerminal(ctx contextT, p sdk.CreateTerminalRequest) (sdk.CreateTerminalResponse, error) {
	return sdk.CreateTerminalResponse{}, fmt.Errorf("terminal not supported (capability not advertised)")
}

func (c *clientHandler) KillTerminal(ctx contextT, p sdk.KillTerminalRequest) (sdk.KillTerminalResponse, error) {
	return sdk.KillTerminalResponse{}, fmt.Errorf("terminal not supported")
}

func (c *clientHandler) ReleaseTerminal(ctx contextT, p sdk.ReleaseTerminalRequest) (sdk.ReleaseTerminalResponse, error) {
	return sdk.ReleaseTerminalResponse{}, fmt.Errorf("terminal not supported")
}

func (c *clientHandler) TerminalOutput(ctx contextT, p sdk.TerminalOutputRequest) (sdk.TerminalOutputResponse, error) {
	return sdk.TerminalOutputResponse{}, fmt.Errorf("terminal not supported")
}

func (c *clientHandler) WaitForTerminalExit(ctx contextT, p sdk.WaitForTerminalExitRequest) (sdk.WaitForTerminalExitResponse, error) {
	return sdk.WaitForTerminalExitResponse{}, fmt.Errorf("terminal not supported")
}

// tailBuffer keeps the LAST max bytes written — a crashing agent's useful
// stderr is at the end, and an unbounded buffer on a chatty agent is a leak.
type tailBuffer struct {
	mu  sync.Mutex
	buf []byte
	max int
}

func (t *tailBuffer) Write(p []byte) (int, error) {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.buf = append(t.buf, p...)
	if len(t.buf) > t.max {
		t.buf = t.buf[len(t.buf)-t.max:]
	}
	return len(p), nil
}

func (t *tailBuffer) String() string {
	t.mu.Lock()
	defer t.mu.Unlock()
	return string(t.buf)
}
