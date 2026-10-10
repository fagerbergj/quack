package acp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log/slog"
	"os/exec"
	"slices"
	"strings"
	"sync"
	"syscall"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	oteltrace "go.opentelemetry.io/otel/trace"

	"github.com/fagerbergj/quack/internal/workspace"
)

// contextT keeps the sdk.Client method signatures readable below.
type contextT = context.Context

// procHandle is one running ACP subprocess with its client-side connection.
type procHandle struct {
	cmd  *exec.Cmd
	conn *sdk.ClientSideConnection
	// updatesMu guards pending/stalled. SessionUpdate must never block the SDK's notification goroutine (a full
	// queue tears the connection down); notify is a 1-cap wakeup, not a value queue.
	updatesMu sync.Mutex
	pending   []sdk.SessionUpdate
	stalled   bool
	notify    chan struct{}
	stderr    *tailBuffer
	once      sync.Once
	// sent/received tee the raw JSON-RPC frames over stdin/stdout; emit.go builds invoke_agent from them.
	sent, received *teeBuffer
}

// updatesStallThreshold: past this backlog the round loop can't keep up; logged once so the round
// reads as "slow consumer", not a masked peer disconnect.
const updatesStallThreshold = 64

// pushUpdate appends u without blocking and wakes the round loop. stalled logs once per backlog
// episode, not per streamed token.
func (h *procHandle) pushUpdate(u sdk.SessionUpdate) {
	h.updatesMu.Lock()
	h.pending = append(h.pending, u)
	n := len(h.pending)
	stall := n == updatesStallThreshold && !h.stalled
	if stall {
		h.stalled = true
	}
	h.updatesMu.Unlock()
	if stall {
		slog.Warn("acp: event consumer stalling, updates buffering", "component", "acp", "buffered", n)
	}
	select {
	case h.notify <- struct{}{}:
	default:
	}
}

// drainUpdates returns and clears everything buffered since the last drain.
func (h *procHandle) drainUpdates() []sdk.SessionUpdate {
	h.updatesMu.Lock()
	defer h.updatesMu.Unlock()
	u := h.pending
	h.pending = nil
	h.stalled = false
	return u
}

// traceparentEnv renders the round span as a W3C TRACEPARENT env entry so the subprocess parents
// its OTLP spans under this round. Empty when ctx has no valid span.
func traceparentEnv(ctx context.Context) []string {
	sc := oteltrace.SpanContextFromContext(ctx)
	if !sc.IsValid() {
		return nil
	}
	return []string{fmt.Sprintf("TRACEPARENT=00-%s-%s-%s", sc.TraceID(), sc.SpanID(), sc.TraceFlags())}
}

// wrappedArgv wraps Command through the sandbox seam (workspace.WrapArgv);
// RO adds SkillPaths() plus ExtraRO(), both queried fresh every spawn.
func (a *Agent) wrappedArgv(cwd string, caps workspace.Caps) []string {
	var extraRO []string
	if a.opts.SkillPaths != nil {
		// Copy: SkillPaths() may return a cached slice with spare capacity, and appending in place
		// would race a concurrent spawn reading it.
		extraRO = append([]string(nil), a.opts.SkillPaths()...)
	}
	if a.opts.ExtraRO != nil {
		extraRO = append(extraRO, a.opts.ExtraRO()...)
	}
	return workspace.WrapArgv(cwd, a.opts.Command, caps, extraRO, nil)
}

// spawnEnv: PATH is hermetic (workspace.ChildPath) and TMPDIR tracks this round's caps.ScratchDir. GIT_ASKPASS/
// GIT_SSH_COMMAND=/bin/false and GIT_TERMINAL_PROMPT=0 fail remote auth closed; local/file:// push still works.
func (a *Agent) spawnEnv(caps workspace.Caps) []string {
	env := SpawnEnv(a.opts.Home, a.opts.Env, caps)
	if a.opts.SkillPaths != nil {
		env = mergeSkillPaths(env, a.opts.SkillPaths())
	}
	return env
}

// mergeSkillPaths rewrites PI_ACP_CONFIG's skill_paths in place with a fresh list; the env built once
// at construction (piACPEnv) would otherwise never see a registry update.
func mergeSkillPaths(env []string, skillPaths []string) []string {
	if len(skillPaths) == 0 {
		return env
	}
	out := slices.Clone(env)
	for i, kv := range out {
		rest, ok := strings.CutPrefix(kv, "PI_ACP_CONFIG=")
		if !ok {
			continue
		}
		var cfg map[string]any
		if err := json.Unmarshal([]byte(rest), &cfg); err != nil {
			break
		}
		cfg["skill_paths"] = skillPaths
		if b, err := json.Marshal(cfg); err == nil {
			out[i] = "PI_ACP_CONFIG=" + string(b)
		}
		break
	}
	return out
}

// startLive spawns a real ACP subprocess and wires the ACP connection.
func (a *Agent) startLive(ctx context.Context, cwd string, caps workspace.Caps) (*procHandle, error) {
	h := &procHandle{
		notify:   make(chan struct{}, 1),
		stderr:   &tailBuffer{max: 4096},
		sent:     &teeBuffer{},
		received: &teeBuffer{},
	}
	argv := a.wrappedArgv(cwd, caps)
	cmd := exec.Command(argv[0], argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = append(a.spawnEnv(caps), traceparentEnv(ctx)...)
	// Own process group + group kill + WaitDelay: a grandchild holding our stdout pipe otherwise keeps Wait
	// blocked forever (mirrors workspace.newChildCmd).
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
	// Tee the wire both ways for the ledger's invoke_agent event: the ACP conversation itself, not a summary.
	teedIn := io.MultiWriter(stdin, h.sent)
	teedOut := io.TeeReader(stdout, h.received)
	h.conn = sdk.NewClientSideConnection(&clientHandler{h: h, judge: a.opts.PermissionJudge}, teedIn, teedOut)
	return h, nil
}

// close kills the subprocess and every descendant (workspace.StopChild), then reaps it. Idempotent.
func (h *procHandle) close(log *slog.Logger) {
	h.once.Do(func() {
		if h.cmd == nil {
			return
		}
		_ = workspace.StopChild(h.cmd)
		if err := h.cmd.Wait(); err != nil && log != nil {
			log.Debug("acp subprocess exit", "err", err, "stderr", h.stderr.String())
		}
	})
}

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

// SessionUpdate must never block: it runs on the SDK's single notification goroutine, and stalling its
// bounded queue tears the connection down. pushUpdate only appends and signals, so ctx is unused.
func (c *clientHandler) SessionUpdate(ctx contextT, n sdk.SessionNotification) error {
	c.h.pushUpdate(n.Update)
	return nil
}

// RequestPermission routes the ask to Options.PermissionJudge (the shim hard-blocks push/clone and escalates
// only a .env read). No judge means allow: the single-tenant container is the boundary.
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
