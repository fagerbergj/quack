package acp

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/fagerbergj/quack/internal/workspace"
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
	// sent/received tee the raw JSON-RPC frames this handle's connection
	// exchanges over stdin/stdout - the replay ledger's invoke_agent event
	// (emit.go) is built from these at the end of the round.
	sent, received *teeBuffer
}

// wrappedArgv is the subprocess argv actually exec'd: a.opts.Command wrapped
// through the SAME sandbox seam every other child runs inside
// (workspace.WrapArgv) - RW is cwd's own scope (the node dir), RO adds the
// skill paths opencode needs to read (ExtraRO) on top of the caps' own system
// + exec_path grants. bwrap/none pass Command through unchanged today (see
// WrapArgv's doc).
func (a *Agent) wrappedArgv(cwd string) []string {
	return workspace.WrapArgv(cwd, a.opts.Command, a.opts.Caps, a.opts.ExtraRO, nil)
}

// spawnEnv is the subprocess environment: PATH is HERMETIC in every sandbox
// mode (workspace.ChildPath - the same fixed PATH the gate's own children
// get), never the server's ambient PATH - the toolchain the agent needs to
// RUN is covered by Caps.ExtraPath + the system dirs already in ChildPath, so
// ambient added no reach a leak couldn't also use.
func (a *Agent) spawnEnv() []string {
	return append([]string{
		"PATH=" + workspace.ChildPath(a.opts.Caps),
		"HOME=" + a.opts.Home,
		"TMPDIR=" + workspace.SandboxTmpDir(a.opts.Caps),
		"NO_COLOR=1",
	}, a.opts.Env...)
}

// start spawns the agent subprocess rooted at cwd and wires the ACP connection.
func (a *Agent) start(cwd string) (*procHandle, error) {
	h := &procHandle{
		updates:  make(chan sdk.SessionUpdate, 64),
		stop:     make(chan struct{}),
		stderr:   &tailBuffer{max: 4096},
		sent:     &teeBuffer{},
		received: &teeBuffer{},
	}
	argv := a.wrappedArgv(cwd)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = a.spawnEnv()
	// Own process group + group kill + WaitDelay: the exact hang class from the
	// v0.5.2 run_command incident - a grandchild holding our stdout pipe keeps
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
	// Tee the wire: everything quack writes to the subprocess's stdin and
	// everything it reads back off stdout, for the replay ledger's
	// invoke_agent event (emit.go) - the ACP conversation itself, not just a
	// summary of it.
	teedIn := io.MultiWriter(stdin, h.sent)
	teedOut := io.TeeReader(stdout, h.received)
	h.conn = sdk.NewClientSideConnection(&clientHandler{h: h, judge: a.opts.PermissionJudge}, teedIn, teedOut)
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
// capabilities, so those methods only fire on a misbehaving agent - they refuse.
type clientHandler struct {
	h     *procHandle
	judge func(ctx context.Context, toolName, title string, input map[string]any) (bool, string)
}

var _ sdk.Client = (*clientHandler)(nil)

func (c *clientHandler) SessionUpdate(ctx contextT, n sdk.SessionNotification) error {
	select {
	case c.h.updates <- n.Update:
	case <-c.h.stop:
	case <-ctx.Done():
	}
	return nil
}

// RequestPermission routes the ask to the safety judge (Options.
// PermissionJudge) - the ACP twin of the native guard ladder's judge tier.
// The generated permission config already allows everything a round
// legitimately needs, so an ask is by construction the exceptional case
// (a directory escape, a .env read, opencode's doom_loop detector); the
// judge decides it with context. No judge configured ⇒ allow, matching the
// single-tenant container-is-the-boundary posture.
func (c *clientHandler) RequestPermission(ctx contextT, p sdk.RequestPermissionRequest) (sdk.RequestPermissionResponse, error) {
	title := ""
	if p.ToolCall.Title != nil {
		title = *p.ToolCall.Title
	}
	toolName := "tool"
	if p.ToolCall.Kind != nil {
		toolName = string(*p.ToolCall.Kind)
	}
	allow, reason := true, "no safety judge configured; container boundary applies"
	if c.judge != nil {
		input, _ := p.ToolCall.RawInput.(map[string]any)
		allow, reason = c.judge(ctx, toolName, title, input)
	}
	slog.Info("acp permission ask judged", "component", "acp",
		"tool_call", string(p.ToolCall.ToolCallId), "title", title, "allow", allow, "reason", reason)
	want := sdk.PermissionOptionKindAllowOnce
	if !allow {
		want = sdk.PermissionOptionKindRejectOnce
	}
	for _, o := range p.Options {
		if o.Kind == want {
			return sdk.RequestPermissionResponse{Outcome: sdk.RequestPermissionOutcome{
				Selected: &sdk.RequestPermissionOutcomeSelected{OptionId: o.OptionId},
			}}, nil
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

// tailBuffer keeps the LAST max bytes written - a crashing agent's useful
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
