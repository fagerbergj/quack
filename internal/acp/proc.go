package acp

import (
	"context"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	sdk "github.com/coder/acp-go-sdk"

	"github.com/fagerbergj/quack/internal/ledger"
	"github.com/fagerbergj/quack/internal/replay"
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
	// replayIO is set instead of cmd for a replayed round (startReplay) - its
	// pump goroutine needs closing on every exit path, same as a real
	// subprocess needs killing.
	replayIO io.Closer
}

// wrappedArgv is the subprocess argv actually exec'd: a.opts.Command wrapped
// through the SAME sandbox seam every other child runs inside
// (workspace.WrapArgv) - RW (or RO, per caps.ReadOnly - #754) is cwd's own
// scope (the node dir), RO adds the skill paths opencode needs to read
// (ExtraRO) on top of the caps' own system + exec_path grants. landlock
// applies them as a ruleset, bwrap as identity bind mounts (#921); `none`
// passes Command through unchanged.
func (a *Agent) wrappedArgv(cwd string, extraRO []string, caps workspace.Caps) []string {
	return workspace.WrapArgv(cwd, a.opts.Command, caps, append(append([]string{}, a.opts.ExtraRO...), extraRO...), nil)
}

// spawnEnv is the subprocess environment: PATH is HERMETIC in every sandbox
// mode (workspace.ChildPath - the same fixed PATH the gate's own children
// get), never the server's ambient PATH - the toolchain the agent needs to
// RUN is covered by Caps.ExtraPath + the system dirs already in ChildPath, so
// ambient added no reach a leak couldn't also use. caps is THIS round's
// effective caps (ReadOnly/ScratchDir already resolved by the caller, same as
// wrappedArgv takes) - TMPDIR must track caps.ScratchDir's per-node grant, not
// the agent's static opts.Caps, or every round would share one scratch dir.
//
// The GIT_* trio strips the child's authority to authenticate to any real
// remote (#936) - GIT_ASKPASS/GIT_SSH_COMMAND point at /bin/false so an HTTPS
// or SSH credential prompt fails closed instead of hanging or succeeding, and
// GIT_TERMINAL_PROMPT=0 kills git's own fallback prompt. `git push` itself
// stays fully allowed: it works against a local/file:// remote (the test
// suite's own target) and merely can't authenticate anywhere else. This is
// independent of internal/vetting's gate-owned push, which builds its own env
// from scratch (pushGitEnv) and is never touched here.
func (a *Agent) spawnEnv(caps workspace.Caps) []string {
	env := []string{
		"PATH=" + workspace.ChildPath(caps),
		"HOME=" + a.opts.Home,
		"TMPDIR=" + workspace.SandboxTmpDir(caps),
		"NO_COLOR=1",
		"GIT_ASKPASS=/bin/false",
		"GIT_SSH_COMMAND=/bin/false",
		"GIT_TERMINAL_PROMPT=0",
	}
	if opts := workspace.SandboxJavaToolOptions(caps); opts != "" {
		env = append(env, "JAVA_TOOL_OPTIONS="+opts)
	}
	return append(env, a.opts.Env...)
}

// start spawns the agent subprocess rooted at cwd and wires the ACP
// connection - or, when Options.Replay is set, wires the SAME connection
// machinery against a recorded conversation instead (startReplay): no
// subprocess, no opencode binary (#604). Fork-replay (#605): when the
// session is in fork mode and this round's stream goes live (startReplay
// returns a *replay.ForkSignal), start falls through to startLive - the
// SAME real-subprocess path a never-replayed round takes, so "live" for ACP
// needs no separate delegate object, only the opts every round already
// carries (Command, Env, Caps, ...).
func (a *Agent) start(ctx context.Context, cwd string, extraRO []string, caps workspace.Caps) (*procHandle, error) {
	if a.opts.Replay != nil {
		h, err := a.startReplay(ctx)
		var fs *replay.ForkSignal
		if errors.As(err, &fs) {
			a.log.Info("acp round forked to live", "reason", fs.Reason, "stream", fs.Stream.String())
			return a.startLive(ctx, cwd, extraRO, caps)
		}
		return h, err
	}
	return a.startLive(ctx, cwd, extraRO, caps)
}

// startLive spawns a real opencode subprocess and wires the ACP connection -
// the only path before #605 added fork-replay's live fallback.
func (a *Agent) startLive(ctx context.Context, cwd string, extraRO []string, caps workspace.Caps) (*procHandle, error) {
	h := &procHandle{
		updates:  make(chan sdk.SessionUpdate, 64),
		stop:     make(chan struct{}),
		stderr:   &tailBuffer{max: 4096},
		sent:     &teeBuffer{},
		received: &teeBuffer{},
	}
	argv := a.wrappedArgv(cwd, extraRO, caps)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = a.spawnEnv(caps)
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

// startReplay resolves this round's recorded invoke_agent entry (the SAME
// ledger.Coords seam inference.NewReplayModel and the tools' replay stubs
// read - ledger.CoordsFromContext) and wires the ACP connection over a
// replayAgentIO instead of a real subprocess's pipes: h.cmd stays nil (close
// then has nothing to kill/wait on), so the gate's view of this round is
// reproduced with no opencode binary at all.
func (a *Agent) startReplay(ctx context.Context) (*procHandle, error) {
	sent, received, err := a.opts.Replay.NextInvokeAgent(ledger.CoordsFromContext(ctx), a.name)
	if err != nil {
		return nil, fmt.Errorf("acp: replay: %w", err)
	}
	h := &procHandle{
		updates:  make(chan sdk.SessionUpdate, 64),
		stop:     make(chan struct{}),
		stderr:   &tailBuffer{max: 4096},
		sent:     &teeBuffer{},
		received: &teeBuffer{},
	}
	rio := newReplayAgentIO(sent, received)
	h.replayIO = rio
	teedIn := io.MultiWriter(rio, h.sent)
	teedOut := io.TeeReader(rio, h.received)
	h.conn = sdk.NewClientSideConnection(&clientHandler{h: h, judge: a.opts.PermissionJudge}, teedIn, teedOut)
	return h, nil
}

// close kills the subprocess's whole process group and reaps it - for a
// replayed round (h.cmd nil; nothing was ever spawned) it instead closes
// replayIO, unblocking its pump goroutine. Idempotent.
func (h *procHandle) close(log *slog.Logger) {
	h.once.Do(func() {
		close(h.stop)
		if h.replayIO != nil {
			_ = h.replayIO.Close()
		}
		if h.cmd == nil {
			return
		}
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
