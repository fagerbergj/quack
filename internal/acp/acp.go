// Package acp runs an external coding agent as an ACP subprocess adapted to an
// ADK agent, one subprocess per worker round.
// ponytail: process-per-round re-reads context each round - keep the process
// alive per node if round startup ever matters.
package acp

import (
	"context"
	"errors"
	"fmt"
	"iter"
	"log/slog"
	"strings"
	"time"

	sdk "github.com/coder/acp-go-sdk"
	"go.opentelemetry.io/otel/attribute"
	adkagent "google.golang.org/adk/v2/agent"
	"google.golang.org/adk/v2/session"
	"google.golang.org/genai"

	"github.com/fagerbergj/quack/internal/otelobs"
	"github.com/fagerbergj/quack/internal/replay"
	"github.com/fagerbergj/quack/internal/vetting"
	"github.com/fagerbergj/quack/internal/workspace"
)

// Options configures one external ACP agent.
type Options struct {
	Command []string // argv to spawn, e.g. ["opencode", "acp"]
	Env     []string
	Caps    workspace.Caps
	ExtraRO []string
	Home    string
	// ponytail: computed once, not per round, so its date footer can go stale
	// on a long-lived server; raise it to a func() string if that ever bites.
	Preamble        string
	Jail            *workspace.Jail
	UserID          string
	Worktree        func(ctx context.Context, userID, chatID, parentNodeID, nodeID string) (dir string, err error)
	StartTimeout    time.Duration
	IdleTimeout     time.Duration
	PermissionJudge func(ctx context.Context, toolName, title string, input map[string]any) (allow bool, reason string)
	Replay          *replay.Session
}

// Agent is an adkagent.Agent backed by an external ACP subprocess.
type Agent struct {
	adkagent.Agent
	name string
	opts Options
	log  *slog.Logger
}

// New builds an ACP-backed agent.
func New(name, description string, opts Options) (*Agent, error) {
	if len(opts.Command) == 0 {
		return nil, errors.New("acp: empty command")
	}
	if opts.Jail == nil {
		return nil, errors.New("acp: no workspace jail configured")
	}
	if opts.StartTimeout <= 0 {
		opts.StartTimeout = 60 * time.Second
	}
	if opts.IdleTimeout <= 0 {
		opts.IdleTimeout = 10 * time.Minute
	}
	a := &Agent{name: name, opts: opts, log: slog.With("component", "acp", "agent", name)}
	inner, err := adkagent.New(adkagent.Config{
		Name:        name,
		Description: description,
		Run:         a.run,
	})
	if err != nil {
		return nil, fmt.Errorf("acp: %w", err)
	}
	a.Agent = inner
	return a, nil
}

// run is the plain-agent path for Run outside a workflow node.
func (a *Agent) run(ic adkagent.InvocationContext) iter.Seq2[*session.Event, error] {
	return a.runPrompt(ic, contentText(ic.UserContent()))
}

// RunNode is the node-runner path the gate's RunNode drives (vetting
// runWorkerNode): nodeInput is the fully-assembled round prompt.
func (a *Agent) RunNode(ctx adkagent.Context, nodeInput any) iter.Seq2[*session.Event, error] {
	return a.runPrompt(ctx, inputText(nodeInput))
}

// resolveNode derives the node's working directory, memory-MCP credential,
// and GitHub context-dir grant from the advisor-thread marker in the prompt.
// memSecret is resolved separately in the memSessions registry - the
// advisor-thread token never doubles as the MCP bearer credential.
func (a *Agent) resolveNode(ctx context.Context, prompt string) (cwd, memSecret, ctxDir string, err error) {
	token, ok := vetting.ParseAdvisorThread(prompt)
	if !ok {
		return "", "", "", errors.New("acp: prompt carries no workspace-scope marker (is this agent running outside the gate?)")
	}
	at, ok := vetting.LookupAdvisorThread(token)
	if !ok {
		return "", "", "", fmt.Errorf("acp: advisor thread %q not registered", token)
	}
	if a.opts.Jail != nil {
		ctxDir, _ = a.opts.Jail.Resolve(a.opts.UserID, at.SessionID, workspace.ContextDirScope)
	}
	if at.WorktreeParent != "" {
		if a.opts.Worktree == nil {
			return "", "", "", fmt.Errorf("acp: node %q needs a git worktree but no worktree executor is configured", at.NodeID)
		}
		cwd, err = a.opts.Worktree(ctx, a.opts.UserID, at.SessionID, at.WorktreeParent, at.WorkspaceNodeID)
		return cwd, at.MemSecret, ctxDir, err
	}
	cwd, err = a.opts.Jail.EnsureDir(a.opts.UserID, at.SessionID, workspace.NodeDir(at.WorkspaceNodeID))
	return cwd, at.MemSecret, ctxDir, err
}

// runPrompt is one full round: spawn, handshake, prompt, stream translation, shutdown.
func (a *Agent) runPrompt(ctx adkagent.InvocationContext, prompt string) iter.Seq2[*session.Event, error] {
	return func(yield func(*session.Event, error) bool) {
		if strings.TrimSpace(prompt) == "" {
			yield(nil, errors.New("acp: empty prompt"))
			return
		}
		cwd, memSecret, ctxDir, err := a.resolveNode(ctx, prompt)
		if err != nil {
			yield(nil, err)
			return
		}
		outbound := environmentBlock(ctx, cwd, a.opts.Caps) + "\n\n" + prompt
		if a.opts.Preamble != "" {
			outbound = a.opts.Preamble + "\n\n" + outbound
		}
		var extraRO []string
		if ctxDir != "" {
			extraRO = []string{ctxDir}
		}
		stopped := false
		err = a.round(ctx, cwd, memSecret, extraRO, outbound, func(spec eventSpec) bool {
			if !yield(a.newEvent(ctx, spec), nil) {
				stopped = true
				return false
			}
			return true
		})
		if err != nil && !stopped {
			yield(nil, err)
		}
	}
}

// promptDone carries the Prompt RPC's outcome off the goroutine in round.
type promptDone struct {
	resp sdk.PromptResponse
	err  error
}

// round drives one subprocess round. Separated from runPrompt for testability.
func (a *Agent) round(ctx context.Context, cwd, memSecret string, extraRO []string, outbound string, emit func(eventSpec) bool) (err error) {
	ctx, roundSpan := otelobs.Start(ctx, "acp.round", attribute.String("agent", a.name), attribute.String("cwd", cwd))
	defer func() { otelobs.End(roundSpan, err) }()

	spawnCtx, spawnSpan := otelobs.Start(ctx, "acp.spawn", attribute.String("agent", a.name))
	_ = spawnCtx
	h, err := a.start(ctx, cwd, extraRO)
	otelobs.End(spawnSpan, err)
	if err != nil {
		return err
	}
	defer h.close(a.log)
	defer func() { emitInvokeAgent(ctx, a.name, h.sent, h.received, err) }()

	ictx, cancelInit := context.WithTimeout(ctx, a.opts.StartTimeout)
	defer cancelInit()
	handshakeCtx, handshakeSpan := otelobs.Start(ctx, "acp.handshake", attribute.String("agent", a.name))
	_ = handshakeCtx
	initResp, err := h.conn.Initialize(ictx, sdk.InitializeRequest{
		ProtocolVersion:    sdk.ProtocolVersionNumber,
		ClientCapabilities: sdk.ClientCapabilities{},
	})
	if err != nil {
		otelobs.End(handshakeSpan, err)
		return fmt.Errorf("acp: initialize: %w%s", err, h.stderrTail())
	}
	mcpServers := memoryMCPServers(memSecret, initResp.AgentCapabilities)
	memSession, _ := vetting.LookupMemSession(memSecret)
	toolNames := mcpToolNames(memSession, len(mcpServers) > 0)
	a.log.Info("acp negotiated capabilities", "mcp_http", initResp.AgentCapabilities.McpCapabilities.Http,
		"mcp_sse", initResp.AgentCapabilities.McpCapabilities.Sse, "mcp_acp", initResp.AgentCapabilities.McpCapabilities.Acp,
		"mcp_surface_offered", len(mcpServers) > 0, "has_mem_secret", memSecret != "", "mcp_tools", toolNames)
	sess, err := h.conn.NewSession(ictx, sdk.NewSessionRequest{Cwd: cwd, McpServers: mcpServers})
	if err != nil {
		otelobs.End(handshakeSpan, err)
		return fmt.Errorf("acp: session/new: %w%s", err, h.stderrTail())
	}
	handshakeSpan.SetAttributes(attribute.String("session_id", string(sess.SessionId)))
	otelobs.End(handshakeSpan, nil)
	a.log.Info("acp round started", "cwd", cwd, "session", sess.SessionId)

	finalPrompt := mcpToolsBlock(toolNames) + "\n\n" + outbound

	done := make(chan promptDone, 1)
	_, promptSpan := otelobs.Start(ctx, "acp.prompt", attribute.String("agent", a.name), attribute.String("session_id", string(sess.SessionId)))
	defer promptSpan.End() // safety net for the relay-stopped/cancel exits below; the done-branch sets the real status first
	go func() {
		resp, perr := h.conn.Prompt(context.Background(), sdk.PromptRequest{
			SessionId: sess.SessionId,
			Prompt:    []sdk.ContentBlock{sdk.TextBlock(finalPrompt)},
		})
		done <- promptDone{resp, perr}
	}()

	tr := newTranslator(cwd)
	relay := func(u sdk.SessionUpdate) bool {
		for _, spec := range tr.translate(u) {
			if !emit(spec) {
				return false
			}
		}
		return true
	}

	idleTimer := time.NewTimer(a.opts.IdleTimeout)
	defer idleTimer.Stop()
	resetIdle := func() {
		if !idleTimer.Stop() {
			select {
			case <-idleTimer.C:
			default:
			}
		}
		idleTimer.Reset(a.opts.IdleTimeout)
	}

	for {
		select {
		case u := <-h.updates:
			resetIdle()
			if !relay(u) {
				return nil
			}
		case d := <-done:
			for {
				select {
				case u := <-h.updates:
					if !relay(u) {
						return nil
					}
					continue
				default:
				}
				break
			}
			if d.err != nil {
				otelobs.End(promptSpan, d.err)
				return fmt.Errorf("acp: prompt: %w%s", d.err, h.stderrTail())
			}
			if d.resp.StopReason == sdk.StopReasonRefusal {
				refusalErr := errors.New("acp: agent refused the prompt")
				otelobs.End(promptSpan, refusalErr)
				return refusalErr
			}
			final := finalSpec(tr)
			a.log.Info("acp round done", "stop", string(d.resp.StopReason), "answer_len", len(final.parts[0].Text))
			promptSpan.SetAttributes(attribute.String("stop_reason", string(d.resp.StopReason)))
			otelobs.End(promptSpan, nil)
			emit(final)
			return nil
		case <-ctx.Done():
			a.gracefulCancel(h, sess.SessionId, done)
			return ctx.Err()
		case <-idleTimer.C:
			a.gracefulCancel(h, sess.SessionId, done)
			return fmt.Errorf("acp: no activity for %s - treating opencode as wedged%s", a.opts.IdleTimeout, h.stderrTail())
		}
	}
}

// mcpToolNames lists the exact MCP tool names this round offers, derived from
// the same MemSession the loopback server resolves per-request.
func mcpToolNames(sess vetting.MemSession, offered bool) []string {
	if !offered {
		return nil
	}
	var names []string
	add := func(tool string) { names = append(names, mcpServerName+"_"+tool) }
	if sess.Memory != nil {
		add(toolLoadMemory)
		add(toolStageMemory)
	}
	if sess.Review != nil {
		add(toolStageReviewComment)
		add(toolListReviewComments)
		add(toolUnstageReviewComment)
		add(toolStageReview)
	}
	if sess.PRStage != nil {
		if sess.ExistingPR {
			add(toolStagePush)
		} else {
			add(toolStagePR)
		}
	}
	return names
}

// mcpToolsBlock renders the offered MCP tool names as a round-start fact.
func mcpToolsBlock(names []string) string {
	if len(names) == 0 {
		return "MCP tools available to you this round: none."
	}
	return "MCP tools available to you this round:\n  " + strings.Join(names, ", ")
}

// gracefulCancel sends session/cancel and waits for the prompt goroutine to acknowledge.
func (a *Agent) gracefulCancel(h *procHandle, sessID sdk.SessionId, done <-chan promptDone) {
	cctx, cancel := context.WithTimeout(context.Background(), cancelGrace)
	defer cancel()
	_ = h.conn.Cancel(cctx, sdk.CancelNotification{SessionId: sessID})
	select {
	case <-done:
	case <-cctx.Done():
	}
}

// cancelGrace bounds how long a cancelled round waits for acknowledgement. A var so tests can shorten it.
var cancelGrace = 5 * time.Second

// newEvent wraps one translated spec as a session event with an explicit Branch.
func (a *Agent) newEvent(ctx adkagent.InvocationContext, spec eventSpec) *session.Event {
	ev := session.NewEvent(ctx, ctx.InvocationID())
	ev.Author = a.name
	ev.Branch = ctx.Branch()
	ev.Partial = spec.partial
	ev.Content = &genai.Content{Role: "model", Parts: spec.parts}
	if spec.usage != nil {
		ev.UsageMetadata = spec.usage
	}
	return ev
}

// contentText flattens a content's plain text parts.
func contentText(c *genai.Content) string {
	if c == nil {
		return ""
	}
	var b strings.Builder
	for _, p := range c.Parts {
		if p != nil && p.Text != "" {
			b.WriteString(p.Text)
		}
	}
	return b.String()
}

// inputText extracts the prompt from a node input.
func inputText(in any) string {
	switch v := in.(type) {
	case string:
		return v
	case *genai.Content:
		return contentText(v)
	default:
		return ""
	}
}
